// These tests reach svc.metadata and reuse setupTestSnapshotService and
// createTestVolume, all package-internal.
//
//test:in-package
package handlers_ec2_snapshot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil/recordingstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The document no longer lives inside the prefix DeleteSnapshot sweeps, so it
// has to be deleted explicitly. Miss that and DescribeSnapshots keeps returning
// a snapshot whose chunks are gone.
func TestDeleteSnapshot_LeavesNoDocument(t *testing.T) {
	ctx := context.Background()
	svc, store := setupTestSnapshotService(t)
	createTestVolume(t, svc, store, "vol-del", 8)

	snap, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-del")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snap.SnapshotId}, testAccountID)
	require.NoError(t, err)

	listed, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, listed.Snapshots, "a deleted snapshot must leave no document behind")

	_, err = svc.metadata.GetSnapshot(ctx, testAccountID, aws.StringValue(snap.SnapshotId))
	require.Error(t, err)
}

// The system account partitions like any tenant: GlobalAccountID is a valid
// account ID, so nothing on this path needs a special case. This fails if
// IsAccountID is ever tightened in a way that rejects "000000000000".
func TestSnapshot_SystemAccountIsAnOrdinaryAccount(t *testing.T) {
	ctx := context.Background()
	svc, store := setupTestSnapshotService(t)
	createTestVolumeForAccount(t, svc, store, "vol-sys", 8, utils.GlobalAccountID)

	snap, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-sys")}, utils.GlobalAccountID)
	require.NoError(t, err)

	listed, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{}, utils.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Snapshots, 1)
	assert.Equal(t, aws.StringValue(snap.SnapshotId), aws.StringValue(listed.Snapshots[0].SnapshotId))

	_, err = svc.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snap.SnapshotId}, utils.GlobalAccountID)
	require.NoError(t, err)
}

// A caller with no account cannot name a prefix, so the listing fails rather
// than widening to every account's snapshots. The strict form reports the
// underlying failure; the tolerant one maps it to an internal error.
func TestDescribeSnapshots_RefusesACallerWithNoAccount(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{}, "")
	require.Error(t, err)

	_, err = svc.DescribeSnapshotsStrict(ctx, &ec2.DescribeSnapshotsInput{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid EBS metadata account ID")
}

// A document that survives the delete leaves DescribeSnapshots returning a
// snapshot whose chunks are gone, so the failure must surface rather than be
// logged and swallowed.
func TestDeleteSnapshot_DocumentDeleteFailureSurfaces(t *testing.T) {
	ctx := context.Background()
	objects := &deleteFailingStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(),
		failPrefix: "spinifex/ebsmetadata/v2/snapshots/"}
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, objects, nil)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	createTestVolume(t, svc, objects.MemoryObjectStore, "vol-delfail", 8)

	snap, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-delfail")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snap.SnapshotId}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// deleteFailingStore fails DeleteObject for keys under failPrefix, leaving
// every other operation intact.
type deleteFailingStore struct {
	*objectstore.MemoryObjectStore

	failPrefix string
}

func (s *deleteFailingStore) DeleteObject(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if strings.HasPrefix(aws.StringValue(input.Key), s.failPrefix) {
		return nil, errors.New("simulated document delete failure")
	}
	return s.MemoryObjectStore.DeleteObject(ctx, input)
}

// Isolation must be enforced *by* the read, not by a filter after it. A
// whole-tree listing plus a correct in-memory filter would return the same
// snapshots, so this asserts the access pattern: the listing prefix is the
// caller's own, and no key under another account's prefix is fetched.
func TestDescribeSnapshots_TouchesNoOtherAccountsPrefix(t *testing.T) {
	ctx := context.Background()
	objects := recordingstore.New()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, objects, nil)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))

	for _, snapshot := range []ebsmetadata.Snapshot{
		{SnapshotID: "snap-mine1", VolumeID: "vol-1", OwnerID: testAccountID, State: "completed"},
		{SnapshotID: "snap-theirs1", VolumeID: "vol-2", OwnerID: otherAccountID, State: "completed"},
	} {
		require.NoError(t, svc.metadata.PutSnapshot(ctx, snapshot))
	}

	objects.Reset()
	listed, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Snapshots, 1)
	assert.Equal(t, "snap-mine1", aws.StringValue(listed.Snapshots[0].SnapshotId))

	// Spelled out rather than derived: this is the layout claim itself.
	assert.False(t, objects.TouchedPrefix("spinifex/ebsmetadata/v2/snapshots/"+otherAccountID+"/"),
		"a describe must not read or list another account's prefix: %v %v", objects.ListPrefixes(), objects.Gets())
}

// The dependency check must stay whole-cluster. Launching from a system AMI
// writes a root volume owned by the launching tenant whose SnapshotID is the
// system snapshot, so a clone routinely lives outside the snapshot owner's
// prefix. Scope the check and the delete strips chunks a live tenant volume
// still reads.
func TestDeleteSnapshot_BlockedByACloneInAnotherAccount(t *testing.T) {
	ctx := context.Background()
	svc, store := setupTestSnapshotService(t)
	createTestVolumeForAccount(t, svc, store, "vol-system", 8, utils.GlobalAccountID)

	snap, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-system")}, utils.GlobalAccountID)
	require.NoError(t, err)

	seedVolumeDocument(t, store, ebsmetadata.Volume{
		VolumeID:   "vol-tenant-root",
		TenantID:   otherAccountID,
		SnapshotID: aws.StringValue(snap.SnapshotId),
	})

	_, err = svc.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snap.SnapshotId}, utils.GlobalAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotInUse, err.Error())
}
