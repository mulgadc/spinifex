package handlers_ec2_snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata/vblegacy"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "111122223333"
const otherAccountID = "444455556666"

// setupTestSnapshotService creates a snapshot service with in-memory storage for testing.
func setupTestSnapshotService(t *testing.T) (*SnapshotServiceImpl, *objectstore.MemoryObjectStore) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			AccessKey: "test-owner-123",
		},
	}

	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	return svc, store
}

// createTestVolume creates a test volume in the mock store
// The real S3 stores VBState (which wraps VolumeConfig), so we match that format.
func createTestVolume(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string, sizeGiB int) {
	volumeState := vblegacy.VBState{
		VolumeConfig: vblegacy.VolumeConfig{
			VolumeMetadata: vblegacy.VolumeMetadata{
				SizeGiB:          uint64(sizeGiB),
				AvailabilityZone: "us-east-1a",
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)

	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String("test-bucket"),
		Key:         aws.String(volumeID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

// TestCreateSnapshot tests creating a snapshot from a volume.
func TestCreateSnapshot(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create a test volume first
	volumeID := "vol-test123"
	createTestVolume(t, store, volumeID, 100)

	// Create snapshot
	result, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId:    aws.String(volumeID),
		Description: aws.String("Test snapshot"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("snapshot"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("test-snap")},
				},
			},
		},
	}, testAccountID)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, strings.HasPrefix(*result.SnapshotId, "snap-"))
	assert.Equal(t, volumeID, *result.VolumeId)
	assert.Equal(t, int64(100), *result.VolumeSize)
	assert.Equal(t, "completed", *result.State)
	assert.Equal(t, "100%", *result.Progress)
	assert.Equal(t, "Test snapshot", *result.Description)
	assert.Equal(t, testAccountID, *result.OwnerId)

	// Verify tags
	assert.Len(t, result.Tags, 1)
	assert.Equal(t, "Name", *result.Tags[0].Key)
	assert.Equal(t, "test-snap", *result.Tags[0].Value)
}

// TestCreateSnapshot_MissingVolumeId tests creating a snapshot without volume ID.
func TestCreateSnapshot_MissingVolumeId(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// TestCreateSnapshot_VolumeZeroSize tests that creating a snapshot from a volume with zero SizeGiB fails.
func TestCreateSnapshot_VolumeZeroSize(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Store a volume config with SizeGiB == 0
	volumeState := vblegacy.VBState{
		VolumeConfig: vblegacy.VolumeConfig{
			VolumeMetadata: vblegacy.VolumeMetadata{
				SizeGiB: 0,
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)

	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-zerosize/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	_, err = svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-zerosize"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorServerInternal)
}

// createEncryptedTestVolume seeds config.json as an at-rest encryption envelope
// ({payload, authtag}) wrapping a VBState, matching what an encrypted volume
// persists. The snapshot handler must unwrap it via StateBody, not decode raw.
func createEncryptedTestVolume(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string, sizeGiB int) {
	inner := vblegacy.VBState{
		EncryptionEnabled: true,
		VolumeConfig: vblegacy.VolumeConfig{
			VolumeMetadata: vblegacy.VolumeMetadata{
				SizeGiB:          uint64(sizeGiB),
				AvailabilityZone: "us-east-1a",
			},
		},
	}
	payload, err := json.Marshal(inner)
	require.NoError(t, err)

	envelope, err := json.Marshal(map[string]any{
		"payload": json.RawMessage(payload),
		"authtag": "deadbeefdeadbeefdeadbeefdeadbeef",
	})
	require.NoError(t, err)

	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(envelope)),
	})
	require.NoError(t, err)
}

// TestCreateSnapshot_EncryptedVolumeEnvelope verifies an encrypted volume (whose
// config.json is an envelope) snapshots successfully instead of 500ing on a
// zero-decoded VBState.
func TestCreateSnapshot_EncryptedVolumeEnvelope(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	volumeID := "vol-enc-snap"
	createEncryptedTestVolume(t, store, volumeID, 100)

	result, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, int64(100), *result.VolumeSize)
	assert.True(t, *result.Encrypted)
}

// TestSnapshotInUseByVolumes_EncryptedVolume verifies the membership scan sees an
// encrypted volume's source SnapshotID through the envelope (a raw decode would
// miss it, letting an in-use snapshot be deleted).
func TestSnapshotInUseByVolumes_EncryptedVolume(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	inner := vblegacy.VBState{
		EncryptionEnabled: true,
		VolumeConfig: vblegacy.VolumeConfig{
			VolumeMetadata: vblegacy.VolumeMetadata{SizeGiB: 50, SnapshotID: "snap-src"},
		},
	}
	payload, err := json.Marshal(inner)
	require.NoError(t, err)
	envelope, err := json.Marshal(map[string]any{
		"payload": json.RawMessage(payload),
		"authtag": "deadbeefdeadbeefdeadbeefdeadbeef",
	})
	require.NoError(t, err)
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-fromsnap/config.json"),
		Body:   strings.NewReader(string(envelope)),
	})
	require.NoError(t, err)

	inUse, err := svc.snapshotInUseByVolumes(context.Background(), "snap-src")
	require.NoError(t, err)
	assert.True(t, inUse, "encrypted volume's SnapshotID must be visible through the envelope")
}

// TestCreateSnapshot_VolumeNotFound tests creating a snapshot from non-existent volume.
func TestCreateSnapshot_VolumeNotFound(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-nonexistent"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidVolumeNotFound)
}

// TestDescribeSnapshots tests listing all snapshots.
func TestDescribeSnapshots(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volumes
	createTestVolume(t, store, "vol-1", 50)
	createTestVolume(t, store, "vol-2", 100)

	// Create multiple snapshots
	snap1, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId:    aws.String("vol-1"),
		Description: aws.String("Snapshot 1"),
	}, testAccountID)
	require.NoError(t, err)

	snap2, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId:    aws.String("vol-2"),
		Description: aws.String("Snapshot 2"),
	}, testAccountID)
	require.NoError(t, err)

	// Describe all snapshots
	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Snapshots, 2)

	// Verify snapshot IDs are present
	snapshotIDs := make(map[string]bool)
	for _, snap := range result.Snapshots {
		snapshotIDs[*snap.SnapshotId] = true
	}
	assert.True(t, snapshotIDs[*snap1.SnapshotId])
	assert.True(t, snapshotIDs[*snap2.SnapshotId])
}

func TestDescribeSnapshotsStrict_RejectsPartialMetadataResults(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	_, err := store.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(GetSnapshotKey("snap-corrupt")),
		Body:   strings.NewReader("not-json"),
	})
	require.NoError(t, err)

	// The customer-facing list retains its historical best-effort behavior.
	out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Snapshots)

	// Reconciliation cannot interpret that partial result as authoritative
	// absence, so its internal lookup surfaces the metadata failure.
	_, err = svc.DescribeSnapshotsStrict(t.Context(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptSnapshotMetadata)
}

// TestDescribeSnapshots_ByID tests listing specific snapshots by ID.
func TestDescribeSnapshots_ByID(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volume
	createTestVolume(t, store, "vol-1", 50)

	// Create multiple snapshots
	snap1, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Describe only the first snapshot
	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap1.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Snapshots, 1)
	assert.Equal(t, *snap1.SnapshotId, *result.Snapshots[0].SnapshotId)
}

// TestDescribeSnapshots_NotFound asserts that naming a specific, nonexistent
// snapshot ID errors, matching real AWS: naming a missing resource is a
// failure, unlike a --filters query that simply matches nothing (see
// TestDescribeSnapshots_FilterNoResults).
func TestDescribeSnapshots_NotFound(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestVolume(t, store, "vol-1", 50)

	snap1, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap1.SnapshotId, aws.String("snap-0000000000000000")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidSnapshot.NotFound")
}

// TestDescribeSnapshots_Empty tests listing snapshots when none exist.
func TestDescribeSnapshots_Empty(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Snapshots)
}

// TestDescribeSnapshots_AccountScoping tests that account A cannot see account B's snapshots.
func TestDescribeSnapshots_AccountScoping(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)

	// Account A creates a snapshot
	snapA, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Account B creates a snapshot
	snapB, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, otherAccountID)
	require.NoError(t, err)

	// Account A should only see its own snapshot
	resultA, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, resultA.Snapshots, 1)
	assert.Equal(t, *snapA.SnapshotId, *resultA.Snapshots[0].SnapshotId)

	// Account B should only see its own snapshot
	resultB, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, otherAccountID)
	require.NoError(t, err)
	require.Len(t, resultB.Snapshots, 1)
	assert.Equal(t, *snapB.SnapshotId, *resultB.Snapshots[0].SnapshotId)
}

// TestDeleteSnapshot tests deleting a snapshot.
func TestDeleteSnapshot(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volume and snapshot
	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify snapshot exists
	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 1)

	// Delete snapshot
	_, err = svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	// Verify snapshot is gone: describing it by its now-deleted ID errors,
	// matching real AWS (naming a specific missing resource is a failure).
	_, err = svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidSnapshot.NotFound")

	// An unfiltered list, unlike naming the deleted ID, is still empty with no error.
	result, err = svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, result.Snapshots)
}

// TestDeleteSnapshot_WrongAccount tests that account B cannot delete account A's snapshot.
func TestDeleteSnapshot_WrongAccount(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Account B tries to delete account A's snapshot — should fail
	_, err = svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, otherAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorUnauthorizedOperation)

	// Verify snapshot still exists
	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 1)
}

// TestDeleteSnapshot_InUseByVolume tests that deleting a snapshot fails when a volume was created from it.
func TestDeleteSnapshot_InUseByVolume(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create a test volume and snapshot
	createTestVolume(t, store, "vol-source", 50)
	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-source"),
	}, testAccountID)
	require.NoError(t, err)

	// Create a volume that references this snapshot (simulates CreateVolume from snapshot)
	volumeState := vblegacy.VBState{
		VolumeConfig: vblegacy.VolumeConfig{
			VolumeMetadata: vblegacy.VolumeMetadata{
				SizeGiB:    50,
				SnapshotID: *snap.SnapshotId,
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-cloned/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	// Attempt to delete the snapshot — should fail
	_, err = svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotInUse)

	// Verify snapshot still exists
	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 1)
}

// TestDeleteSnapshot_NotFound tests deleting a non-existent snapshot.
func TestDeleteSnapshot_NotFound(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String("snap-nonexistent"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
}

// TestDeleteSnapshot_MissingID tests deleting without snapshot ID.
func TestDeleteSnapshot_MissingID(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// Without a provider a copy would need snapshot metadata under the new ID that
// the control plane cannot re-seal on an encrypted volume. Writing only the
// control-plane config produced a snapshot that described fine and then failed
// at attach, so this path refuses instead of producing one.
func TestCopySnapshot_UnsupportedWithoutProvider(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CopySnapshot(context.Background(), &ec2.CopySnapshotInput{
		SourceSnapshotId: snap.SnapshotId,
		Description:      aws.String("Copied snapshot"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnsupportedOperation, err.Error())

	// The refusal must not leave a half-created snapshot behind.
	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 1, "only the source snapshot should exist")
}

// TestCopySnapshot_WrongAccount tests that account B cannot copy account A's snapshot.
func TestCopySnapshot_WrongAccount(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)
	_, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Get the snapshot ID
	result, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, result.Snapshots, 1)

	// Account B tries to copy account A's snapshot — should fail
	_, err = svc.CopySnapshot(context.Background(), &ec2.CopySnapshotInput{
		SourceSnapshotId: result.Snapshots[0].SnapshotId,
	}, otherAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorUnauthorizedOperation)
}

// TestCopySnapshot_NotFound tests copying a non-existent snapshot.
func TestCopySnapshot_NotFound(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CopySnapshot(context.Background(), &ec2.CopySnapshotInput{
		SourceSnapshotId: aws.String("snap-nonexistent"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
}

// TestCopySnapshot_MissingSourceID tests copying without source snapshot ID.
func TestCopySnapshot_MissingSourceID(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CopySnapshot(context.Background(), &ec2.CopySnapshotInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// setupTestNATSKV creates a NATS JetStream test server and returns a KV bucket for testing.
func setupTestNATSKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: KVBucketVolumeSnapshots,
	})
	require.NoError(t, err)
	return kv
}

func TestAddSnapshotRef(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	require.NoError(t, svc.addSnapshotRef(t.Context(), "vol-1", "snap-a"))
	require.NoError(t, svc.addSnapshotRef(t.Context(), "vol-1", "snap-b"))

	entry, err := kv.Get(t.Context(), "vol-1")
	require.NoError(t, err)
	var snapshots []string
	require.NoError(t, json.Unmarshal(entry.Value(), &snapshots))
	assert.Equal(t, []string{"snap-a", "snap-b"}, snapshots)
}

func TestRemoveSnapshotRef(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	require.NoError(t, svc.addSnapshotRef(t.Context(), "vol-1", "snap-a"))
	require.NoError(t, svc.addSnapshotRef(t.Context(), "vol-1", "snap-b"))

	// Remove one
	require.NoError(t, svc.removeSnapshotRef(t.Context(), "vol-1", "snap-a"))

	entry, err := kv.Get(t.Context(), "vol-1")
	require.NoError(t, err)
	var snapshots []string
	require.NoError(t, json.Unmarshal(entry.Value(), &snapshots))
	assert.Equal(t, []string{"snap-b"}, snapshots)

	// Remove last — key should be deleted
	require.NoError(t, svc.removeSnapshotRef(t.Context(), "vol-1", "snap-b"))

	_, err = kv.Get(t.Context(), "vol-1")
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

func TestRemoveSnapshotRef_NonExistentKey(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	// Should not error on non-existent key
	require.NoError(t, svc.removeSnapshotRef(t.Context(), "vol-nonexistent", "snap-x"))
}

func TestRemoveSnapshotRefForCleanupSurvivesCancellation(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}
	require.NoError(t, svc.addSnapshotRef(t.Context(), "vol-1", "snap-a"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, svc.removeSnapshotRefForCleanup(ctx, "vol-1", "snap-a"))

	_, err := kv.Get(t.Context(), "vol-1")
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

func TestVolumeHasSnapshots(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	// No entry → false
	has, err := svc.volumeHasSnapshots(t.Context(), "vol-1")
	require.NoError(t, err)
	assert.False(t, has)

	// Add one → true
	require.NoError(t, svc.addSnapshotRef(t.Context(), "vol-1", "snap-a"))
	has, err = svc.volumeHasSnapshots(t.Context(), "vol-1")
	require.NoError(t, err)
	assert.True(t, has)

	// Remove it → false
	require.NoError(t, svc.removeSnapshotRef(t.Context(), "vol-1", "snap-a"))
	has, err = svc.volumeHasSnapshots(t.Context(), "vol-1")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestKVNilFallback(t *testing.T) {
	svc := &SnapshotServiceImpl{snapKV: nil}

	// All methods should be no-ops when KV is nil
	require.NoError(t, svc.addSnapshotRef(t.Context(), "vol-1", "snap-a"))
	require.NoError(t, svc.removeSnapshotRef(t.Context(), "vol-1", "snap-a"))
	has, err := svc.volumeHasSnapshots(t.Context(), "vol-1")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestCreateSnapshot_WritesKVEntry(t *testing.T) {
	kv := setupTestNATSKV(t)
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			AccessKey: "test-owner-123",
		},
	}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil, kv)

	volumeID := "vol-kvtest"
	createTestVolume(t, store, volumeID, 10)

	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify KV entry exists
	has, err := svc.volumeHasSnapshots(t.Context(), volumeID)
	require.NoError(t, err)
	assert.True(t, has)

	// Verify snapshot ID is in the list
	entry, err := kv.Get(t.Context(), volumeID)
	require.NoError(t, err)
	var snapshots []string
	require.NoError(t, json.Unmarshal(entry.Value(), &snapshots))
	assert.Contains(t, snapshots, *snap.SnapshotId)
}

func TestDeleteSnapshot_RemovesKVEntry(t *testing.T) {
	kv := setupTestNATSKV(t)
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			AccessKey: "test-owner-123",
		},
	}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil, kv)

	volumeID := "vol-kvdelete"
	createTestVolume(t, store, volumeID, 10)

	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, testAccountID)
	require.NoError(t, err)

	// Delete the snapshot
	_, err = svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	// KV should now be empty for this volume
	has, err := svc.volumeHasSnapshots(t.Context(), volumeID)
	require.NoError(t, err)
	assert.False(t, has)
}

// TestCreateSnapshot_CrossAccountVolumeRejected tests that snapshotting another account's volume is rejected.
func TestCreateSnapshot_CrossAccountVolumeRejected(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create a volume owned by testAccountID (via TenantID)
	volumeID := "vol-owned-by-alpha"
	volumeState := vblegacy.VBState{
		VolumeConfig: vblegacy.VolumeConfig{
			VolumeMetadata: vblegacy.VolumeMetadata{
				SizeGiB:          100,
				TenantID:         testAccountID,
				AvailabilityZone: "us-east-1a",
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	// Another account tries to snapshot the volume — should fail
	_, err = svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, otherAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidVolumeNotFound)

	// Same account can snapshot — should succeed
	result, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, testAccountID, *result.OwnerId)
}

// TestCreateSnapshot_PrePhase4VolumeAllowed tests that volumes without TenantID (pre-phase4) are allowed.
func TestCreateSnapshot_PrePhase4VolumeAllowed(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create a volume with no TenantID (pre-phase4)
	volumeID := "vol-legacy"
	createTestVolume(t, store, volumeID, 50)

	// Any account can snapshot — backward compatibility
	result, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, otherAccountID)
	require.NoError(t, err)
	assert.Equal(t, otherAccountID, *result.OwnerId)
}

// --- DescribeSnapshots filter tests ---

// createTestSnapshot creates a snapshot and returns its ID.
func createTestSnapshot(t *testing.T, svc *SnapshotServiceImpl, store *objectstore.MemoryObjectStore, volumeID string, sizeGiB int, tags map[string]string) string {
	t.Helper()
	createTestVolume(t, store, volumeID, sizeGiB)

	tagSpecs := []*ec2.TagSpecification{}
	if len(tags) > 0 {
		ec2Tags := make([]*ec2.Tag, 0, len(tags))
		for k, v := range tags {
			ec2Tags = append(ec2Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		tagSpecs = append(tagSpecs, &ec2.TagSpecification{
			ResourceType: aws.String("snapshot"),
			Tags:         ec2Tags,
		})
	}

	result, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId:          aws.String(volumeID),
		TagSpecifications: tagSpecs,
	}, testAccountID)
	require.NoError(t, err)
	return *result.SnapshotId
}

func TestDescribeSnapshots_FilterByStatus(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-1", 10, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("completed")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)

	out, err = svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("pending")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Snapshots)
}

func TestDescribeSnapshots_FilterByVolumeId(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-a", 10, nil)
	createTestSnapshot(t, svc, store, "vol-b", 20, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-a")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)
	assert.Equal(t, "vol-a", *out.Snapshots[0].VolumeId)
}

func TestDescribeSnapshots_FilterByVolumeSize(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-small", 10, nil)
	createTestSnapshot(t, svc, store, "vol-big", 100, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-size"), Values: []*string{aws.String("100")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)
	assert.Equal(t, int64(100), *out.Snapshots[0].VolumeSize)
}

func TestDescribeSnapshots_FilterBySnapshotId(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapID := createTestSnapshot(t, svc, store, "vol-1", 10, nil)
	createTestSnapshot(t, svc, store, "vol-2", 20, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("snapshot-id"), Values: []*string{aws.String(snapID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)
	assert.Equal(t, snapID, *out.Snapshots[0].SnapshotId)
}

func TestDescribeSnapshots_FilterByOwnerId(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-1", 10, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("owner-id"), Values: []*string{aws.String(testAccountID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)

	out, err = svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("owner-id"), Values: []*string{aws.String("999999999999")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Snapshots)
}

func TestDescribeSnapshots_FilterMultipleValues_OR(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-a", 10, nil)
	createTestSnapshot(t, svc, store, "vol-b", 20, nil)
	createTestSnapshot(t, svc, store, "vol-c", 30, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-a"), aws.String("vol-c")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 2)
}

func TestDescribeSnapshots_FilterMultipleFilters_AND(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-a", 10, nil)
	createTestSnapshot(t, svc, store, "vol-b", 20, nil)

	// Both match
	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-a")}},
			{Name: aws.String("volume-size"), Values: []*string{aws.String("10")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)

	// Mismatch
	out, err = svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-a")}},
			{Name: aws.String("volume-size"), Values: []*string{aws.String("20")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Snapshots)
}

func TestDescribeSnapshots_FilterUnknownName_Error(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testAccountID)
	assert.Error(t, err)
}

func TestDescribeSnapshots_FilterWildcard(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-prod-1", 10, nil)
	createTestSnapshot(t, svc, store, "vol-staging-1", 20, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-prod-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)
}

func TestDescribeSnapshots_FilterNoResults(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-1", 10, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-nonexistent")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Snapshots)
}

func TestDescribeSnapshots_FilterByTag(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-tagged", 10, map[string]string{"Env": "prod"})
	createTestSnapshot(t, svc, store, "vol-untagged", 20, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Env"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)
}

func TestDescribeSnapshots_FilterNoFilters(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-1", 10, nil)
	createTestSnapshot(t, svc, store, "vol-2", 20, nil)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 2)
}

// TestCreateSnapshot_PredastoreInitFails exercises snapshotVolume past the nil-host guard.
// The viperblock S3 backend fails at Init() when the test server returns 403, covering the
// snapshotVolume body (S3Config build, LoadViperblockMasterKey, viperblock.New, Backend.Init)
// and the CreateSnapshot error return path.
func TestCreateSnapshot_PredastoreInitFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Host:   srv.URL,
			Region: "us-east-1",
			Bucket: "test-bucket",
		},
	}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	createTestVolume(t, store, "vol-initfail", 10)

	_, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-initfail"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// s3MockServer returns a test server that passes ListObjectsV2 (Backend.Init) but
// returns 404 for every other request (GetObject, PutObject, etc.).
func s3MockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "list-type=2") {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
				`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
				`<Name>test-bucket</Name><KeyCount>0</KeyCount>`+
				`<MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
			`<Error><Code>NoSuchKey</Code>`+
			`<Message>The specified key does not exist.</Message></Error>`)
	}))
}

func TestSnapshotVolume_EncryptionKeyLoadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Host:   srv.URL,
			Region: "us-east-1",
			Bucket: "test-bucket",
		},
		Viperblock: config.ViperblockConfig{
			EncryptionKeyFile: "/nonexistent-snap-test-key.bin",
		},
	}
	svc := NewSnapshotServiceImplWithStore(cfg, objectstore.NewMemoryObjectStore(), nil)

	err := svc.snapshotVolume("vol-enckey", "snap-enckey", 10*1024*1024*1024)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load encryption key")
}

func TestSnapshotVolume_LoadStateFails(t *testing.T) {
	srv := s3MockServer(t)
	defer srv.Close()

	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Host:      srv.URL,
			Region:    "us-east-1",
			Bucket:    "test-bucket",
			AccessKey: "test-key",
			SecretKey: "test-secret",
		},
	}
	svc := NewSnapshotServiceImplWithStore(cfg, objectstore.NewMemoryObjectStore(), nil)

	err := svc.snapshotVolume("vol-lsf", "snap-lsf", 10*1024*1024*1024)
	require.Error(t, err)
}

func TestCreateDeleteSnapshot_UsesInjectedProvider(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: "vol-provider", TenantID: testAccountID, CapacityGiB: 4, State: "available", AvailabilityZone: "us-east-1a", ProviderHandle: "memory://volume/vol-provider",
	}))
	_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-provider", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4 * 1024 * 1024 * 1024}, AvailabilityZone: "us-east-1a"})
	require.NoError(t, err)

	snapshot, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-provider")}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	cfgStored, err := svc.getSnapshotConfig(aws.StringValue(snapshot.SnapshotId))
	require.NoError(t, err)
	assert.Equal(t, "memory://snapshot/"+aws.StringValue(snapshot.SnapshotId), cfgStored.ProviderHandle)
	_, err = svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{SnapshotId: snapshot.SnapshotId}, testAccountID)
	require.NoError(t, err)
}

// A provider-managed clone records its source snapshot only in ebsmetadata, so
// the legacy config.json scan cannot see it. Deleting the snapshot underneath
// one would strip chunks the clone still reads.
func TestDeleteSnapshot_Provider_BlockedByCloneRecordedInMetadata(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)

	require.NoError(t, svc.metadata.PutVolume(ctx, ebsmetadata.Volume{
		VolumeID: "vol-source", TenantID: testAccountID, CapacityGiB: 4,
		State: "available", AvailabilityZone: "us-east-1a", ProviderHandle: "memory://volume/vol-source",
	}))
	_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-source",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4 * 1024 * 1024 * 1024}, AvailabilityZone: "us-east-1a",
	})
	require.NoError(t, err)

	snapshot, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-source")}, testAccountID)
	require.NoError(t, err)
	snapshotID := aws.StringValue(snapshot.SnapshotId)

	// The clone exists only as a control-plane document, as CreateVolume's
	// provider branch writes it.
	require.NoError(t, svc.metadata.PutVolume(ctx, ebsmetadata.Volume{
		VolumeID: "vol-clone", TenantID: testAccountID, CapacityGiB: 4, State: "available",
		AvailabilityZone: "us-east-1a", SnapshotID: snapshotID, ProviderHandle: "memory://volume/vol-clone",
	}))

	_, err = svc.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidSnapshotInUse)

	// Once the clone is gone the snapshot is deletable again.
	require.NoError(t, svc.metadata.DeleteVolume(ctx, "vol-clone"))
	_, err = svc.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)}, testAccountID)
	require.NoError(t, err)
}

// snapshotMetadataFailingObjectStore fails PutObject for any snapshot
// metadata.json write and records the last such key, so a test can recover
// the randomly generated snapshot ID CreateSnapshot attempted to persist.
type snapshotMetadataFailingObjectStore struct {
	objectstore.ObjectStore

	attemptedKey string
}

func (s *snapshotMetadataFailingObjectStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	key := aws.StringValue(input.Key)
	if strings.HasSuffix(key, "/metadata.json") {
		s.attemptedKey = key
		return nil, errors.New("simulated metadata write failure")
	}
	return s.ObjectStore.PutObject(ctx, input)
}

// TestCreateSnapshot_Provider_RollbackOnMetadataWriteFailure covers
// CreateSnapshot's rollback path: when persisting the snapshot config fails
// after the provider has already created the snapshot, CreateSnapshot must
// delete the just-created provider snapshot rather than orphan it.
func TestCreateSnapshot_Provider_RollbackOnMetadataWriteFailure(t *testing.T) {
	store := &snapshotMetadataFailingObjectStore{ObjectStore: objectstore.NewMemoryObjectStore()}
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)

	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: "vol-rollback", TenantID: testAccountID, CapacityGiB: 4, State: "available",
		AvailabilityZone: "us-east-1a", ProviderHandle: "memory://volume/vol-rollback",
	}))
	_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-rollback",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4 * 1024 * 1024 * 1024}, AvailabilityZone: "us-east-1a",
	})
	require.NoError(t, err)

	_, err = svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-rollback")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServerInternal)
	require.NotEmpty(t, store.attemptedKey, "the snapshot metadata write must have been attempted")

	snapshotID := strings.TrimSuffix(store.attemptedKey, "/metadata.json")
	_, err = provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-rollback"})
	require.NoError(t, err, "the source volume must be unaffected by the snapshot rollback")

	// Recreating the same snapshot ID against a different source volume only
	// succeeds if the original snapshot no longer exists (MemoryProvider
	// returns already_exists for a conflicting source volume on a live
	// snapshot ID), proving the rollback actually deleted it rather than
	// merely leaving it orphaned but reachable.
	_, err = provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-rollback-check", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 * 1024 * 1024 * 1024},
	})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: "vol-rollback-check",
	})
	require.NoError(t, err, "rollback must have deleted the just-created provider snapshot")
}

// providerSnapshotService builds a snapshot service backed by the memory
// provider with one volume registered, ready to snapshot.
func providerSnapshotService(t *testing.T, volumeID string) (*SnapshotServiceImpl, *ebsprovider.MemoryProvider) {
	t.Helper()
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: testAccountID, CapacityGiB: 4, State: "available",
		AvailabilityZone: "us-east-1a", ProviderHandle: "memory://volume/" + volumeID,
	}))
	_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 4 * 1024 * 1024 * 1024},
		AvailabilityZone: "us-east-1a",
	})
	require.NoError(t, err)
	return svc, provider
}

// The copy must exist in the provider under its own ID, not just as a
// control-plane document. That gap is what made the previous implementation
// describe a snapshot that failed at attach.
func TestCopySnapshot_Provider_CreatesBackendSnapshot(t *testing.T) {
	ctx := context.Background()
	svc, provider := providerSnapshotService(t, "vol-copysrc")

	src, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-copysrc")}, testAccountID)
	require.NoError(t, err)

	out, err := svc.CopySnapshot(ctx, &ec2.CopySnapshotInput{
		SourceSnapshotId: src.SnapshotId, Description: aws.String("copied"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.SnapshotId)
	newID := aws.StringValue(out.SnapshotId)
	assert.NotEqual(t, aws.StringValue(src.SnapshotId), newID)

	got, ok := provider.Snapshot(newID)
	require.True(t, ok, "the copy must exist in the provider, not only in the control plane")
	assert.Equal(t, "vol-copysrc", got.SourceVolumeID)

	stored, err := svc.getSnapshotConfig(newID)
	require.NoError(t, err)
	assert.Equal(t, "vol-copysrc", stored.VolumeID)
	assert.Equal(t, "copied", stored.Description)
	assert.Equal(t, testAccountID, stored.OwnerID)
}

// Both the copy and its source must describe. This service is built without
// JetStream, so it covers the control-plane documents only — the snapshot-ref
// rollback needs a KV-backed service and is not exercised here.
func TestCopySnapshot_Provider_CopyAndSourceBothDescribe(t *testing.T) {
	ctx := context.Background()
	svc, _ := providerSnapshotService(t, "vol-copyref")

	src, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-copyref")}, testAccountID)
	require.NoError(t, err)
	out, err := svc.CopySnapshot(ctx, &ec2.CopySnapshotInput{SourceSnapshotId: src.SnapshotId}, testAccountID)
	require.NoError(t, err)

	listed, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	ids := make([]string, 0, len(listed.Snapshots))
	for _, s := range listed.Snapshots {
		ids = append(ids, aws.StringValue(s.SnapshotId))
	}
	assert.Contains(t, ids, aws.StringValue(out.SnapshotId))
	assert.Contains(t, ids, aws.StringValue(src.SnapshotId))
}

// The copy inherits the source description when the caller supplies none,
// rather than landing with an empty one.
func TestCopySnapshot_Provider_InheritsSourceDescription(t *testing.T) {
	ctx := context.Background()
	svc, _ := providerSnapshotService(t, "vol-copydesc")

	src, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-copydesc"), Description: aws.String("original text"),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.CopySnapshot(ctx, &ec2.CopySnapshotInput{SourceSnapshotId: src.SnapshotId}, testAccountID)
	require.NoError(t, err)
	stored, err := svc.getSnapshotConfig(aws.StringValue(out.SnapshotId))
	require.NoError(t, err)
	assert.Equal(t, "original text", stored.Description)
}

// A provider failure must leave nothing behind: no control-plane document and
// no entry in the volume's snapshot list.
func TestCopySnapshot_Provider_FailureLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	svc, provider := providerSnapshotService(t, "vol-copyfail")

	src, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-copyfail")}, testAccountID)
	require.NoError(t, err)

	svc.SetEBSProvider(&copyFailingProvider{EBSProvider: provider})
	_, err = svc.CopySnapshot(ctx, &ec2.CopySnapshotInput{SourceSnapshotId: src.SnapshotId}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	listed, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, listed.Snapshots, 1, "only the source snapshot should exist")
}

// Ownership is checked before the copy runs, so another tenant's snapshot is
// refused rather than duplicated into the caller's account.
func TestCopySnapshot_Provider_ForeignSnapshotRefused(t *testing.T) {
	ctx := context.Background()
	svc, provider := providerSnapshotService(t, "vol-copyown")

	src, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-copyown")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CopySnapshot(ctx, &ec2.CopySnapshotInput{SourceSnapshotId: src.SnapshotId}, "999988887777")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())

	_, ok := provider.Snapshot(aws.StringValue(src.SnapshotId))
	require.True(t, ok, "the source must survive a refused copy")
}

// copyFailingProvider fails only CopySnapshot, leaving every other operation
// intact so the rollback path is what the test exercises.
type copyFailingProvider struct {
	ebsprovider.EBSProvider
}

func (p *copyFailingProvider) CopySnapshot(context.Context, ebsprovider.CopySnapshotRequest) (*ebsprovider.Snapshot, error) {
	return nil, errors.New("provider copy failed")
}
