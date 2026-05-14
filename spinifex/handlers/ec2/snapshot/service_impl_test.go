package handlers_ec2_snapshot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "111122223333"
const otherAccountID = "444455556666"

// setupTestSnapshotService creates a snapshot service with in-memory storage for testing
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
	volumeState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				SizeGiB:          uint64(sizeGiB),
				IsEncrypted:      false,
				AvailabilityZone: "us-east-1a",
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String("test-bucket"),
		Key:         aws.String(volumeID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

// TestCreateSnapshot tests creating a snapshot from a volume
func TestCreateSnapshot(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create a test volume first
	volumeID := "vol-test123"
	createTestVolume(t, store, volumeID, 100)

	// Create snapshot
	result, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
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

// TestCreateSnapshot_MissingVolumeId tests creating a snapshot without volume ID
func TestCreateSnapshot_MissingVolumeId(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// TestCreateSnapshot_VolumeZeroSize tests that creating a snapshot from a volume with zero SizeGiB fails
func TestCreateSnapshot_VolumeZeroSize(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Store a volume config with SizeGiB == 0
	volumeState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				SizeGiB: 0,
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-zerosize/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	_, err = svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-zerosize"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorServerInternal)
}

// TestCreateSnapshot_VolumeNotFound tests creating a snapshot from non-existent volume
func TestCreateSnapshot_VolumeNotFound(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-nonexistent"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidVolumeNotFound)
}

// TestDescribeSnapshots tests listing all snapshots
func TestDescribeSnapshots(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volumes
	createTestVolume(t, store, "vol-1", 50)
	createTestVolume(t, store, "vol-2", 100)

	// Create multiple snapshots
	snap1, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:    aws.String("vol-1"),
		Description: aws.String("Snapshot 1"),
	}, testAccountID)
	require.NoError(t, err)

	snap2, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:    aws.String("vol-2"),
		Description: aws.String("Snapshot 2"),
	}, testAccountID)
	require.NoError(t, err)

	// Describe all snapshots
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{}, testAccountID)
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

// TestDescribeSnapshots_ByID tests listing specific snapshots by ID
func TestDescribeSnapshots_ByID(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volume
	createTestVolume(t, store, "vol-1", 50)

	// Create multiple snapshots
	snap1, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Describe only the first snapshot
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap1.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Snapshots, 1)
	assert.Equal(t, *snap1.SnapshotId, *result.Snapshots[0].SnapshotId)
}

// TestDescribeSnapshots_Empty tests listing snapshots when none exist
func TestDescribeSnapshots_Empty(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Snapshots)
}

// TestDescribeSnapshots_AccountScoping tests that account A cannot see account B's snapshots
func TestDescribeSnapshots_AccountScoping(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)

	// Account A creates a snapshot
	snapA, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Account B creates a snapshot
	snapB, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, otherAccountID)
	require.NoError(t, err)

	// Account A should only see its own snapshot
	resultA, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, resultA.Snapshots, 1)
	assert.Equal(t, *snapA.SnapshotId, *resultA.Snapshots[0].SnapshotId)

	// Account B should only see its own snapshot
	resultB, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{}, otherAccountID)
	require.NoError(t, err)
	require.Len(t, resultB.Snapshots, 1)
	assert.Equal(t, *snapB.SnapshotId, *resultB.Snapshots[0].SnapshotId)
}

// TestDeleteSnapshot tests deleting a snapshot
func TestDeleteSnapshot(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volume and snapshot
	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify snapshot exists
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 1)

	// Delete snapshot
	_, err = svc.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	// Verify snapshot is gone
	result, err = svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, result.Snapshots)
}

// TestDeleteSnapshot_WrongAccount tests that account B cannot delete account A's snapshot
func TestDeleteSnapshot_WrongAccount(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Account B tries to delete account A's snapshot — should fail
	_, err = svc.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, otherAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorUnauthorizedOperation)

	// Verify snapshot still exists
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 1)
}

// TestDeleteSnapshot_InUseByVolume tests that deleting a snapshot fails when a volume was created from it
func TestDeleteSnapshot_InUseByVolume(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create a test volume and snapshot
	createTestVolume(t, store, "vol-source", 50)
	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-source"),
	}, testAccountID)
	require.NoError(t, err)

	// Create a volume that references this snapshot (simulates CreateVolume from snapshot)
	volumeState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				SizeGiB:    50,
				SnapshotID: *snap.SnapshotId,
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)
	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-cloned/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	// Attempt to delete the snapshot — should fail
	_, err = svc.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotInUse)

	// Verify snapshot still exists
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{snap.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 1)
}

// TestDeleteSnapshot_NotFound tests deleting a non-existent snapshot
func TestDeleteSnapshot_NotFound(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: aws.String("snap-nonexistent"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
}

// TestDeleteSnapshot_MissingID tests deleting without snapshot ID
func TestDeleteSnapshot_MissingID(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.DeleteSnapshot(&ec2.DeleteSnapshotInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// TestCopySnapshot tests copying a snapshot
func TestCopySnapshot(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volume and snapshot
	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:    aws.String("vol-1"),
		Description: aws.String("Original snapshot"),
	}, testAccountID)
	require.NoError(t, err)

	// Copy snapshot
	copyResult, err := svc.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: snap.SnapshotId,
		Description:      aws.String("Copied snapshot"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, copyResult)
	assert.True(t, strings.HasPrefix(*copyResult.SnapshotId, "snap-"))
	assert.NotEqual(t, *snap.SnapshotId, *copyResult.SnapshotId)

	// Verify both snapshots exist
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, result.Snapshots, 2)
}

// TestCopySnapshot_SetsCallerAsOwner tests that copied snapshot is owned by the caller
func TestCopySnapshot_SetsCallerAsOwner(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Copy snapshot as the same account
	copyResult, err := svc.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	// Verify copied snapshot owner is the caller
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{copyResult.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, result.Snapshots, 1)
	assert.Equal(t, testAccountID, *result.Snapshots[0].OwnerId)
}

// TestCopySnapshot_WrongAccount tests that account B cannot copy account A's snapshot
func TestCopySnapshot_WrongAccount(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	createTestVolume(t, store, "vol-1", 50)
	_, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)

	// Get the snapshot ID
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, result.Snapshots, 1)

	// Account B tries to copy account A's snapshot — should fail
	_, err = svc.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: result.Snapshots[0].SnapshotId,
	}, otherAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorUnauthorizedOperation)
}

// TestCopySnapshot_NotFound tests copying a non-existent snapshot
func TestCopySnapshot_NotFound(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: aws.String("snap-nonexistent"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
}

// TestCopySnapshot_MissingSourceID tests copying without source snapshot ID
func TestCopySnapshot_MissingSourceID(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)

	_, err := svc.CopySnapshot(&ec2.CopySnapshotInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// TestCopySnapshot_PreservesTags tests that tags are copied
func TestCopySnapshot_PreservesTags(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create test volume and snapshot with tags
	createTestVolume(t, store, "vol-1", 50)
	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("snapshot"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Environment"), Value: aws.String("test")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Copy snapshot
	copyResult, err := svc.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	// Verify copied snapshot has tags
	result, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{copyResult.SnapshotId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, result.Snapshots, 1)
	assert.Len(t, result.Snapshots[0].Tags, 1)
	assert.Equal(t, "Environment", *result.Snapshots[0].Tags[0].Key)
	assert.Equal(t, "test", *result.Snapshots[0].Tags[0].Value)
}

// setupTestNATSKV creates a NATS JetStream test server and returns a KV bucket for testing.
func setupTestNATSKV(t *testing.T) nats.KeyValue {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	js, err := nc.JetStream()
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: KVBucketVolumeSnapshots,
	})
	require.NoError(t, err)
	return kv
}

func TestAddSnapshotRef(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	require.NoError(t, svc.addSnapshotRef("vol-1", "snap-a"))
	require.NoError(t, svc.addSnapshotRef("vol-1", "snap-b"))

	entry, err := kv.Get("vol-1")
	require.NoError(t, err)
	var snapshots []string
	require.NoError(t, json.Unmarshal(entry.Value(), &snapshots))
	assert.Equal(t, []string{"snap-a", "snap-b"}, snapshots)
}

func TestRemoveSnapshotRef(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	require.NoError(t, svc.addSnapshotRef("vol-1", "snap-a"))
	require.NoError(t, svc.addSnapshotRef("vol-1", "snap-b"))

	// Remove one
	require.NoError(t, svc.removeSnapshotRef("vol-1", "snap-a"))

	entry, err := kv.Get("vol-1")
	require.NoError(t, err)
	var snapshots []string
	require.NoError(t, json.Unmarshal(entry.Value(), &snapshots))
	assert.Equal(t, []string{"snap-b"}, snapshots)

	// Remove last — key should be deleted
	require.NoError(t, svc.removeSnapshotRef("vol-1", "snap-b"))

	_, err = kv.Get("vol-1")
	assert.ErrorIs(t, err, nats.ErrKeyNotFound)
}

func TestRemoveSnapshotRef_NonExistentKey(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	// Should not error on non-existent key
	require.NoError(t, svc.removeSnapshotRef("vol-nonexistent", "snap-x"))
}

func TestVolumeHasSnapshots(t *testing.T) {
	kv := setupTestNATSKV(t)
	svc := &SnapshotServiceImpl{snapKV: kv}

	// No entry → false
	has, err := svc.volumeHasSnapshots("vol-1")
	require.NoError(t, err)
	assert.False(t, has)

	// Add one → true
	require.NoError(t, svc.addSnapshotRef("vol-1", "snap-a"))
	has, err = svc.volumeHasSnapshots("vol-1")
	require.NoError(t, err)
	assert.True(t, has)

	// Remove it → false
	require.NoError(t, svc.removeSnapshotRef("vol-1", "snap-a"))
	has, err = svc.volumeHasSnapshots("vol-1")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestKVNilFallback(t *testing.T) {
	svc := &SnapshotServiceImpl{snapKV: nil}

	// All methods should be no-ops when KV is nil
	require.NoError(t, svc.addSnapshotRef("vol-1", "snap-a"))
	require.NoError(t, svc.removeSnapshotRef("vol-1", "snap-a"))
	has, err := svc.volumeHasSnapshots("vol-1")
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

	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify KV entry exists
	has, err := svc.volumeHasSnapshots(volumeID)
	require.NoError(t, err)
	assert.True(t, has)

	// Verify snapshot ID is in the list
	entry, err := kv.Get(volumeID)
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

	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, testAccountID)
	require.NoError(t, err)

	// Delete the snapshot
	_, err = svc.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	// KV should now be empty for this volume
	has, err := svc.volumeHasSnapshots(volumeID)
	require.NoError(t, err)
	assert.False(t, has)
}

func TestCopySnapshot_AddsKVEntry(t *testing.T) {
	kv := setupTestNATSKV(t)
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			AccessKey: "test-owner-123",
		},
	}
	svc := NewSnapshotServiceImplWithStore(cfg, store, nil, kv)

	volumeID := "vol-kvcopy"
	createTestVolume(t, store, volumeID, 10)

	snap, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, testAccountID)
	require.NoError(t, err)

	copyResult, err := svc.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	// Both snapshot IDs should be in KV
	entry, err := kv.Get(volumeID)
	require.NoError(t, err)
	var snapshots []string
	require.NoError(t, json.Unmarshal(entry.Value(), &snapshots))
	assert.Contains(t, snapshots, *snap.SnapshotId)
	assert.Contains(t, snapshots, *copyResult.SnapshotId)
	assert.Len(t, snapshots, 2)
}

// TestCreateSnapshot_CrossAccountVolumeRejected tests that snapshotting another account's volume is rejected.
func TestCreateSnapshot_CrossAccountVolumeRejected(t *testing.T) {
	svc, store := setupTestSnapshotService(t)

	// Create a volume owned by testAccountID (via TenantID)
	volumeID := "vol-owned-by-alpha"
	volumeState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				SizeGiB:          100,
				TenantID:         testAccountID,
				AvailabilityZone: "us-east-1a",
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)
	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	// Another account tries to snapshot the volume — should fail
	_, err = svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeID),
	}, otherAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidVolumeNotFound)

	// Same account can snapshot — should succeed
	result, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
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
	result, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
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

	result, err := svc.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:          aws.String(volumeID),
		TagSpecifications: tagSpecs,
	}, testAccountID)
	require.NoError(t, err)
	return *result.SnapshotId
}

func TestDescribeSnapshots_FilterByStatus(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	createTestSnapshot(t, svc, store, "vol-1", 10, nil)

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("completed")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)

	out, err = svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("owner-id"), Values: []*string{aws.String(testAccountID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)

	out, err = svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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
	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-a")}},
			{Name: aws.String("volume-size"), Values: []*string{aws.String("10")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)

	// Mismatch
	out, err = svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	_, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
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

	out, err := svc.DescribeSnapshots(&ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 2)
}
