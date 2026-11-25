package snapshot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
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
	t.Parallel()

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

// A tenant snapshot cannot be reached from another account, so another
// account's unreadable volume record is not consulted and cannot block it.
func TestDeleteSnapshot_ForeignUnreadableVolumeRecordDoesNotBlockATenantSnapshot(t *testing.T) {
	t.Parallel()

	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.VolumeKey(otherAccountID, "vol-foreign-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)

	require.NoError(t, deleteSnapshot(svc, snapshotID))
}

// Account teardown drains snapshots with the pair of calls the storage-stage
// reaper issues: DescribeSnapshots owner-scoped, then DeleteSnapshot for each.
// The incident's state is an unreadable volume record belonging to an unrelated
// tenant, which must leave the inventory empty and the foreign record intact.
func TestDeleteSnapshot_TeardownDrainsTheAccountDespiteAForeignUnreadableRecord(t *testing.T) {
	t.Parallel()

	svc, store := setupTestSnapshotService(t)
	first := unreferencedSnapshot(t, svc, store)
	second := unreferencedSnapshot(t, svc, store)
	foreignKey, err := ebsmetadata.VolumeKey(otherAccountID, "vol-foreign-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, foreignKey)

	listed, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		OwnerIds: []*string{aws.String(testAccountID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Snapshots, 2)

	for _, snap := range listed.Snapshots {
		require.NoError(t, deleteSnapshot(svc, aws.StringValue(snap.SnapshotId)),
			"teardown cannot converge while a snapshot refuses to go")
	}

	remaining, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		OwnerIds: []*string{aws.String(testAccountID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, remaining.Snapshots, "snapshots %s and %s must both be gone", first, second)

	_, err = store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"), Key: aws.String(foreignKey),
	})
	require.NoError(t, err, "the other tenant's record must survive a teardown that is not theirs")
}

// deleteSnapshotAs is deleteSnapshot for a caller other than testAccountID,
// which the global account has to be for the system-snapshot case.
func deleteSnapshotAs(svc *SnapshotServiceImpl, snapshotID, accountID string) error {
	_, err := svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)}, accountID)
	return err
}

// promotedResidue seeds the state PromoteSystemImage leaves when its final
// delete of the tenant-keyed document fails: the snapshot resolves under both
// the promoting tenant and the global account.
func promotedResidue(t *testing.T, svc *SnapshotServiceImpl, store *objectstore.MemoryObjectStore) string {
	t.Helper()
	snapshotID := unreferencedSnapshot(t, svc, store)
	seedSnapshotDocument(t, store, ebsmetadata.Snapshot{
		SnapshotID: snapshotID,
		OwnerID:    awsidentifiers.GlobalAccountID,
		VolumeSize: 50,
	})
	return snapshotID
}

// A snapshot that also resolves under the global account can have been launched
// by any account, so every prefix is still read and a foreign clone refuses it.
//
// No AMI names the snapshot, which is what makes the refusal attributable to
// the volume scan: breadth comes from the global document, so deregistering the
// promoted AMI cannot narrow the scan while the residue and its clones remain.
func TestDeleteSnapshot_PromotedResidueStillScansEveryAccount(t *testing.T) {
	t.Parallel()

	svc, store := setupTestSnapshotService(t)
	snapshotID := promotedResidue(t, svc, store)
	seedVolumeDocument(t, store, ebsmetadata.Volume{
		VolumeID: "vol-foreign-clone", TenantID: otherAccountID, CapacityGiB: 50, SnapshotID: snapshotID,
	})

	amis, err := svc.metadata.ListAMIs(context.Background())
	require.NoError(t, err)
	require.Empty(t, amis, "no AMI may name the snapshot, or the refusal proves nothing")

	code, _ := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	assert.Equal(t, awserrors.ErrorInvalidSnapshotInUse, code)
	requireSnapshotSurvives(t, svc, snapshotID)
}

// Absence of the global document is what permits the narrow scan, so one that
// failed to read has to widen it rather than read as absent.
func TestDeleteSnapshot_UnreadableGlobalSnapshotDocumentScansEveryAccount(t *testing.T) {
	t.Parallel()

	memory := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, memory, nil)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	snapshotID := unreferencedSnapshot(t, svc, memory)

	globalKey, err := ebsmetadata.SnapshotKey(awsidentifiers.GlobalAccountID, snapshotID)
	require.NoError(t, err)
	foreignKey, err := ebsmetadata.VolumeKey(otherAccountID, "vol-foreign-clone")
	require.NoError(t, err)
	seedVolumeDocument(t, memory, ebsmetadata.Volume{
		VolumeID: "vol-foreign-clone", TenantID: otherAccountID, CapacityGiB: 50, SnapshotID: snapshotID,
	})

	// The read of the foreign key is the evidence, because the refusal redacts
	// the volume it found and a short circuit at the global read would produce
	// a refusal of its own.
	store := &readFailingObjectStore{ObjectStore: memory, failing: map[string]bool{globalKey: true}}
	svc = NewSnapshotServiceImplWithStore(cfg, store, nil)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))

	code, _ := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	assert.Equal(t, awserrors.ErrorInvalidSnapshotInUse, code)
	assert.Contains(t, store.readKeys(), foreignKey, "the foreign prefix must have been read")
	requireSnapshotSurvives(t, svc, snapshotID)
}

// A system snapshot's clones routinely live in other accounts, so its own
// delete still reads every prefix and still fails closed on an unreadable one.
func TestDeleteSnapshot_SystemSnapshotStillScansEveryAccount(t *testing.T) {
	t.Parallel()

	svc, store := setupTestSnapshotService(t)
	snapshotID := promotedResidue(t, svc, store)
	key, err := ebsmetadata.VolumeKey(otherAccountID, "vol-foreign-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)

	code, message := clientErrorDetail(t, deleteSnapshotAs(svc, snapshotID, awsidentifiers.GlobalAccountID))
	assert.Equal(t, awserrors.ErrorServerInternal, code)
	assert.Contains(t, message, "could not be read")
}

// A readable clone settles the answer whatever else failed to read, so the
// caller gets the AWS refusal rather than an internal error.
func TestDeleteSnapshot_ReadableCloneWinsOverUnreadableRecord(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	key, err := ebsmetadata.VolumeKey(testAccountID, "vol-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)
	require.NoError(t, svc.metadata.PutAMI(context.Background(), ebsmetadata.AMI{ImageID: "ami-registered", SnapshotID: snapshotID}))

	code, _ := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	assert.Equal(t, awserrors.ErrorInvalidSnapshotInUse, code)
	requireSnapshotSurvives(t, svc, snapshotID)
}

// readFailingObjectStore fails the read of named keys the way predastore fails
// an object whose shards can no longer reconstruct: a 500, not a missing key.
// The metadata collector fetches in parallel, so reads is guarded by mu.
type readFailingObjectStore struct {
	objectstore.ObjectStore

	failing map[string]bool

	mu    sync.Mutex
	reads []string
}

func (s *readFailingObjectStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	key := aws.StringValue(input.Key)
	s.mu.Lock()
	s.reads = append(s.reads, key)
	s.mu.Unlock()
	if s.failing[key] {
		return nil, fmt.Errorf("InternalError: reconstruct %s: every data shard rejected stale_epoch", key)
	}
	return s.ObjectStore.GetObject(ctx, input)
}

func (s *readFailingObjectStore) readKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reads)
}

// The incident this reproduces was an object whose read returns 500 forever,
// not a document that parses badly, and the two reach the collector by
// different paths. A teardown must drain past one belonging to another tenant.
func TestDeleteSnapshot_TeardownDrainsPastAForeignUnreadableObject(t *testing.T) {
	t.Parallel()

	memory := objectstore.NewMemoryObjectStore()
	foreignKey, err := ebsmetadata.VolumeKey(otherAccountID, "vol-0f1e2d3c4b5a69788")
	require.NoError(t, err)
	store := &readFailingObjectStore{ObjectStore: memory, failing: map[string]bool{foreignKey: true}}

	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	snapshotID := unreferencedSnapshot(t, svc, memory)
	seedVolumeDocument(t, memory, ebsmetadata.Volume{
		VolumeID: "vol-0f1e2d3c4b5a69788", TenantID: otherAccountID, CapacityGiB: 50,
	})

	require.NoError(t, deleteSnapshot(svc, snapshotID))
	assert.NotContains(t, store.readKeys(), foreignKey,
		"the foreign object must not be read at all, or a 500 there can still refuse a teardown")
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
	t.Parallel()

	memory := objectstore.NewMemoryObjectStore()
	volumeKey, err := ebsmetadata.VolumeKey(testAccountID, "vol-deleted")
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

// deleteRecordingProvider records the snapshots the service asked it to
// destroy, so a test can assert that none was.
type deleteRecordingProvider struct {
	ebsprovider.EBSProvider

	deleted []string
}

func (p *deleteRecordingProvider) DeleteSnapshot(ctx context.Context, req ebsprovider.DeleteSnapshotRequest) error {
	p.deleted = append(p.deleted, req.SnapshotID)
	return p.EBSProvider.DeleteSnapshot(ctx, req)
}

// Verifies: ADR-0003:S7
//
// A refusal has to leave the blocks, not just the document. Provider deletion
// runs before the document is removed, so a surviving document is not evidence
// on its own that the data survived.
func TestS7_RefusedDeleteDoesNotDestroyTheBlocks(t *testing.T) {
	t.Parallel()

	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	recorder := &deleteRecordingProvider{EBSProvider: svc.provider}
	svc.SetEBSProvider(recorder)
	key, err := ebsmetadata.VolumeKey(testAccountID, "vol-corrupt")
	require.NoError(t, err)
	putCorruptDocument(t, store, key)

	code, _ := clientErrorDetail(t, deleteSnapshot(svc, snapshotID))
	require.Equal(t, awserrors.ErrorServerInternal, code)
	require.Empty(t, recorder.deleted,
		"ADR-0003:S7: automatic failure handling never destroys data. "+
			"DeleteSnapshot refused %s because a volume record could not be read, "+
			"yet asked the provider to delete %v. The refusal must return before "+
			"provider deletion in snapshot/service_impl.go.", snapshotID, recorder.deleted)
	requireSnapshotSurvives(t, svc, snapshotID)
}

// The recorder above observes a delete that is allowed to proceed, so its
// silence in the refusal case is the absence of a call rather than a test
// watching the wrong provider.
func TestS7_RecorderObservesAnUnobstructedDelete(t *testing.T) {
	t.Parallel()

	svc, store := setupTestSnapshotService(t)
	snapshotID := unreferencedSnapshot(t, svc, store)
	recorder := &deleteRecordingProvider{EBSProvider: svc.provider}
	svc.SetEBSProvider(recorder)

	require.NoError(t, deleteSnapshot(svc, snapshotID))
	require.Equal(t, []string{snapshotID}, recorder.deleted)
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
	t.Parallel()

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
			t.Parallel()
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
