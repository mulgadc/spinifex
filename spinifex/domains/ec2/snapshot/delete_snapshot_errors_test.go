package snapshot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putCorruptDocument writes bytes that will not decode under key.
func putCorruptDocument(t *testing.T, store objectstore.ObjectStore, key string) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(key),
		Body:   strings.NewReader("not-json"),
	})
	require.NoError(t, err)
}

// unreferencedSnapshot returns a snapshot nothing in the cluster reads from.
func unreferencedSnapshot(t *testing.T, svc *SnapshotServiceImpl, store *objectstore.MemoryObjectStore) string {
	t.Helper()
	createTestVolume(t, svc, store, "vol-source", 50)
	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-source")}, testAccountID)
	require.NoError(t, err)
	return aws.StringValue(snap.SnapshotId)
}

func deleteSnapshot(svc *SnapshotServiceImpl, snapshotID string) error {
	_, err := svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)}, testAccountID)
	return err
}

func requireSnapshotSurvives(t *testing.T, svc *SnapshotServiceImpl, snapshotID string) {
	t.Helper()
	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{SnapshotIds: []*string{aws.String(snapshotID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Snapshots, 1)
}

// An unreadable volume record cannot be ruled out as a clone of the snapshot,
// so the delete is refused, but with the record named rather than a bare code.
func TestDeleteSnapshot_UnreadableOwnVolumeRecordIsNamed(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.VolumeKey(testAccountID, "vol-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)

	err = deleteSnapshot(svc, snapshotID)
	code, message := clientErrorDetail(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, code)
	assert.Equal(t, "Cannot confirm that snapshot '"+snapshotID+"' is not in use: the record for volume 'vol-corrupt' could not be read", message)
	requireSnapshotSurvives(t, svc, snapshotID)
}

// Another account's unreadable volume blocks the delete without its ID
// reaching the caller.
func TestDeleteSnapshot_UnreadableForeignVolumeRecordIsNotNamed(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.VolumeKey(otherAccountID, "vol-foreign-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)

	err = deleteSnapshot(svc, snapshotID)
	code, message := clientErrorDetail(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, code)
	assert.Equal(t, "Cannot confirm that snapshot '"+snapshotID+"' is not in use: 1 volume record(s) could not be read", message)
	assert.NotContains(t, message, "vol-foreign-corrupt")
	requireSnapshotSurvives(t, svc, snapshotID)
}

// A readable clone settles the answer whatever else failed to read, so the
// caller gets the AWS refusal rather than an internal error.
func TestDeleteSnapshot_ReadableCloneWinsOverUnreadableRecord(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.VolumeKey(testAccountID, "vol-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)
	seedVolumeDocument(t, store, ebsmetadata.Volume{VolumeID: "vol-clone", CapacityGiB: 50, SnapshotID: snapshotID})

	code, message := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	assert.Equal(t, awserrors.ErrorInvalidSnapshotInUse, code)
	assert.Equal(t, "The snapshot '"+snapshotID+"' is in use by volume 'vol-clone'.", message)
}

// An unreadable AMI record blocks the delete, and says so.
func TestDeleteSnapshot_UnreadableAMIRecordIsReported(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.AMIKey("ami-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)

	code, message := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	assert.Equal(t, awserrors.ErrorServerInternal, code)
	assert.Equal(t, "Cannot confirm that snapshot '"+snapshotID+"' is not in use: 1 AMI record(s) could not be read", message)
	requireSnapshotSurvives(t, svc, snapshotID)
}

// A registered AMI naming the snapshot is still the AWS refusal when another
// AMI record is unreadable.
func TestDeleteSnapshot_RegisteredAMIWinsOverUnreadableAMI(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.AMIKey("ami-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)
	require.NoError(t, svc.metadata.PutAMI(context.Background(), ebsmetadata.AMI{ImageID: "ami-registered", SnapshotID: snapshotID}))

	code, _ := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	assert.Equal(t, awserrors.ErrorInvalidSnapshotInUse, code)
}

// A registered AMI naming the snapshot is the AWS refusal even when the volume
// scan, which runs first, could not read a record.
func TestDeleteSnapshot_RegisteredAMIWinsOverUnreadableVolume(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.VolumeKey(otherAccountID, "vol-foreign-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)
	require.NoError(t, svc.metadata.PutAMI(context.Background(), ebsmetadata.AMI{ImageID: "ami-registered", SnapshotID: snapshotID}))

	code, _ := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	assert.Equal(t, awserrors.ErrorInvalidSnapshotInUse, code)
	requireSnapshotSurvives(t, svc, snapshotID)
}

// vanishingObjectStore lists keys whose documents have already been deleted,
// as a listing racing a DeleteVolume or DeregisterImage sees them.
type vanishingObjectStore struct {
	objectstore.ObjectStore

	gone map[string]bool
}

func (s *vanishingObjectStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if key := aws.StringValue(input.Key); s.gone[key] {
		return nil, &objectstore.NoSuchKeyError{Key: key}
	}
	return s.ObjectStore.GetObject(ctx, input)
}

// A record deleted between listing and read is absent, not unreadable, and
// does not stop an unreferenced snapshot from being deleted.
func TestDeleteSnapshot_RecordDeletedMidScanIsAbsent(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	volumeKey, err := ebsmetadata.VolumeKey(otherAccountID, "vol-deleted")
	require.NoError(t, err)
	amiKey, err := ebsmetadata.AMIKey("ami-deregistered")
	require.NoError(t, err)
	store := &vanishingObjectStore{ObjectStore: memory, gone: map[string]bool{volumeKey: true, amiKey: true}}

	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	snapshotID := unreferencedSnapshot(t, svc, memory)
	putCorruptDocument(t, memory, volumeKey)
	putCorruptDocument(t, memory, amiKey)

	require.NoError(t, deleteSnapshot(svc, snapshotID))
}

// deleteFailingProvider fails only DeleteSnapshot, with err.
type deleteFailingProvider struct {
	ebsprovider.EBSProvider

	err error
}

func (p *deleteFailingProvider) DeleteSnapshot(context.Context, ebsprovider.DeleteSnapshotRequest) error {
	return p.err
}

// A provider failure maps to a code the caller can act on, and leaves the
// control-plane document so the delete can be retried.
func TestDeleteSnapshot_ProviderFailureMapsToSpecificError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"provider unavailable", &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeUnavailable, Message: "busy"}, awserrors.ErrorServiceUnavailable},
		{"no provider responding", errors.Join(errors.New("request ebs.provider.v1.snapshot.delete"), nats.ErrNoResponders), awserrors.ErrorServiceUnavailable},
		{"request timed out", context.DeadlineExceeded, awserrors.ErrorServiceUnavailable},
		{"connection closed", nats.ErrConnectionClosed, awserrors.ErrorServiceUnavailable},
		{"nats timeout", nats.ErrTimeout, awserrors.ErrorServiceUnavailable},
		{"backend reports in use", &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeVolumeInUse, Message: "clone"}, awserrors.ErrorInvalidSnapshotInUse},
		{"internal failure", &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInternal, Message: "delete snapshot objects: boom"}, awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := setupTestSnapshotService(t)
			snapshotID := unreferencedSnapshot(t, svc, store)
			svc.SetEBSProvider(&deleteFailingProvider{EBSProvider: svc.provider, err: tc.err})

			code, message := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
			assert.Equal(t, tc.wantCode, code)
			assert.Contains(t, message, snapshotID)
			requireSnapshotSurvives(t, svc, snapshotID)
		})
	}
}
