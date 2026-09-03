package admin

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRemoveBucket    = "test-bucket"
	testRemoveAccountID = "000000000001"
)

// putAMI registers an AMI the only way the control plane knows one: as an
// ebsmetadata document. It writes nothing under ami-<id>/, so an object count
// taken over that prefix sees blocks alone.
func putAMI(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name, owner, snapshotID string) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, testRemoveBucket).PutAMI(t.Context(), ebsmetadata.AMI{
		ImageID:         imageID,
		Name:            name,
		ImageOwnerAlias: owner,
		SnapshotID:      snapshotID,
		VolumeSizeGiB:   8,
	}))
}

// putCorruptAMI writes an undecodable document at the AMI's key, the state the
// salvage path exists for: present, so not not-found, but unreadable.
func putCorruptAMI(t *testing.T, store *objectstore.MemoryObjectStore, imageID string) {
	t.Helper()
	key, err := ebsmetadata.AMIKey(imageID)
	require.NoError(t, err)
	_, err = store.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("{not valid json")),
	})
	require.NoError(t, err)
}

// putAMIBlocks writes a few dummy chunk objects under ami-<id>/ so deletion
// has bulk work and byte counts to verify.
func putAMIBlocks(t *testing.T, store *objectstore.MemoryObjectStore, imageID string, n int, size int) {
	t.Helper()
	body := bytes.Repeat([]byte{0xab}, size)
	for i := range n {
		_, err := store.PutObject(t.Context(), &awss3.PutObjectInput{
			Bucket: aws.String(testRemoveBucket),
			Key:    aws.String(imageID + "/chunks/" + string(rune('a'+i)) + ".dat"),
			Body:   bytes.NewReader(body),
		})
		require.NoError(t, err)
	}
}

func putSnapBlocks(t *testing.T, store *objectstore.MemoryObjectStore, snapID string, n int, size int) {
	t.Helper()
	body := bytes.Repeat([]byte{0xcd}, size)
	for i := range n {
		_, err := store.PutObject(t.Context(), &awss3.PutObjectInput{
			Bucket: aws.String(testRemoveBucket),
			Key:    aws.String(snapID + "/cp/" + string(rune('a'+i)) + ".bin"),
			Body:   bytes.NewReader(body),
		})
		require.NoError(t, err)
	}
}

// putSnapMetadata writes a CopyImage-derived EC2 snapshot metadata that points
// VolumeID back at an admin-imported AMI's ID.
func putSnapMetadata(t *testing.T, store *objectstore.MemoryObjectStore, snapID, volumeID string) {
	t.Helper()
	cfg := &handlers_ec2_snapshot.SnapshotConfig{
		SnapshotID: snapID,
		VolumeID:   volumeID,
		VolumeSize: 8,
		State:      "completed",
	}
	require.NoError(t, handlers_ec2_snapshot.WriteSnapshotConfig(store, testRemoveBucket, snapID, cfg))
}

// putVolume registers a volume document carrying the given SnapshotID.
func putVolume(t *testing.T, store *objectstore.MemoryObjectStore, volID, snapshotID string) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, testRemoveBucket).PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID:    volID,
		TenantID:    testRemoveAccountID,
		SnapshotID:  snapshotID,
		CapacityGiB: 8,
	}))
}

func TestRemoveSystemImage_HappyPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putAMIBlocks(t, store, id, 3, 128)
	putSnapBlocks(t, store, SnapPrefix(id), 2, 16)

	preview, err := PreviewRemoveSystemImage(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.True(t, preview.ConfigPresent)
	assert.True(t, preview.IsSystemOwned)
	assert.Equal(t, "system", preview.Owner)
	// Only the 3 chunks: the document lives outside the ami-<id>/ prefix.
	assert.Equal(t, 3, preview.AMIObjectCount)
	assert.Equal(t, 2, preview.SnapObjectCount)
	assert.True(t, preview.Dependents.Empty())

	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.NoError(t, err)
	assert.Equal(t, preview.AMIObjectCount+preview.SnapObjectCount, res.ObjectsDeleted)
	assert.Equal(t, preview.AMIBytesTotal+preview.SnapBytesTotal, res.BytesDeleted)
	assert.Equal(t, 0, store.Count())
}

// strictDeleteStore reports a missing key on delete, the way predastore does.
// The in-memory store is silently tolerant and AWS S3 answers 204, so neither
// reproduces the backend this actually runs against.
type strictDeleteStore struct {
	*objectstore.MemoryObjectStore
}

var _ objectstore.ObjectStore = (*strictDeleteStore)(nil)

func (s *strictDeleteStore) DeleteObject(ctx context.Context, input *awss3.DeleteObjectInput) (*awss3.DeleteObjectOutput, error) {
	key := aws.StringValue(input.Key)
	if _, err := s.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: input.Bucket, Key: input.Key}); err != nil {
		return nil, &objectstore.NoSuchKeyError{Key: key}
	}
	return s.MemoryObjectStore.DeleteObject(ctx, input)
}

// TestRemoveSystemImage_OrphanedBlocksAreReclaimable covers the state the
// orphan report points operators at: blocks in the object store with no
// document. A live env19 run failed here on NoSuchKey, so the one documented
// way to reclaim an orphan was broken by the very thing that defines one.
func TestRemoveSystemImage_OrphanedBlocksAreReclaimable(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	store := &strictDeleteStore{MemoryObjectStore: memory}
	const id = "ami-orphaned0001"
	putAMIBlocks(t, memory, id, 3, 128)

	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err, "an image whose document is already gone must still have its blocks reclaimable")
	assert.Equal(t, 3, res.ObjectsDeleted)
	assert.Equal(t, 0, memory.Count(), "the stranded blocks must actually be released")
}

func TestRemoveSystemImage_AccountOwned_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-acct-001"
	putAMI(t, store, id, "user-ami", testRemoveAccountID, "snap-acct-001")
	putAMIBlocks(t, store, id, 1, 16)

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), id)
	assert.Contains(t, err.Error(), testRemoveAccountID)
	assert.Contains(t, err.Error(), "account-owned")
	assert.Contains(t, err.Error(), "aws ec2 deregister-image")
	// Objects untouched.
	assert.Equal(t, 2, store.Count())
}

func TestRemoveSystemImage_AccountOwned_ForceBypasses(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-acct-002"
	putAMI(t, store, id, "user-ami", testRemoveAccountID, "snap-acct-002")
	putAMIBlocks(t, store, id, 2, 16)

	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Positive(t, res.ObjectsDeleted)
	assert.Equal(t, 0, store.Count())
}

func TestRemoveSystemImage_MissingConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-missing"
	putAMIBlocks(t, store, id, 1, 16)

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestRemoveSystemImage_CorruptConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-corrupt"
	putCorruptAMI(t, store, id)

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestRemoveSystemImage_DependentVolume_Direct_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	// Direct dependent: a volume launched from the admin import.
	putVolume(t, store, "vol-aaa", SnapPrefix(id))

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	var depErr *DependentError
	require.ErrorAs(t, err, &depErr)
	assert.Contains(t, depErr.Dependents.Volumes, "vol-aaa")
}

func TestRemoveSystemImage_DependentVolume_Transitive_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))

	// CopyImage of the system AMI wrote a snap whose VolumeID points back at the AMI ID.
	const derivedSnap = "snap-derived-001"
	putSnapMetadata(t, store, derivedSnap, id)
	// A volume launched from the copied AMI references the derived snap.
	putVolume(t, store, "vol-bbb", derivedSnap)

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	var depErr *DependentError
	require.ErrorAs(t, err, &depErr)
	assert.Contains(t, depErr.Dependents.Volumes, "vol-bbb")
	assert.Contains(t, depErr.Dependents.Snapshots, derivedSnap)
}

func TestRemoveSystemImage_DependentAMI_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))

	const derivedSnap = "snap-derived-002"
	putSnapMetadata(t, store, derivedSnap, id)
	// Account AMI created via CopyImage; its SnapshotID is the derived snap.
	putAMI(t, store, "ami-acct-002", "copied", testRemoveAccountID, derivedSnap)

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	var depErr *DependentError
	require.ErrorAs(t, err, &depErr)
	assert.Contains(t, depErr.Dependents.AMIs, "ami-acct-002")
}

func TestRemoveSystemImage_Force_OverridesDependents(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-deb13"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putAMIBlocks(t, store, id, 2, 64)
	putVolume(t, store, "vol-orphan", SnapPrefix(id))

	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Positive(t, res.ObjectsDeleted)
	// The dependent volume's document remains; the AMI's is gone.
	metadata := ebsmetadata.NewStore(store, testRemoveBucket)
	_, err = metadata.GetAMI(t.Context(), id)
	require.Error(t, err)
	_, err = metadata.GetVolume(t.Context(), testRemoveAccountID, "vol-orphan")
	require.NoError(t, err)
}

func TestRemoveSystemImage_Salvage_MissingConfig_ForceCleans(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-salvage-1"
	// No config.json — just orphaned blocks.
	putAMIBlocks(t, store, id, 3, 32)
	putSnapBlocks(t, store, SnapPrefix(id), 1, 8)

	// Without --force: NotFound.
	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// With --force: salvage proceeds.
	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, 4, res.ObjectsDeleted)
	assert.Equal(t, 0, store.Count())
}

func TestRemoveSystemImage_Salvage_CorruptConfig_ForceCleans(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-salvage-2"
	putCorruptAMI(t, store, id)
	putAMIBlocks(t, store, id, 1, 8)

	// Without --force.
	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// With --force: the corrupt document and the blocks all go. Only the
	// block is counted; the document is deleted by key, not by prefix walk.
	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, 1, res.ObjectsDeleted)
	assert.Equal(t, 0, store.Count())
}

func TestRemoveSystemImage_IdempotentRerun(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-rerun"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))
	putAMIBlocks(t, store, id, 2, 16)

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.NoError(t, err)
	require.Equal(t, 0, store.Count())

	// Second call without --force should report NotFound.
	_, err = RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// Salvage re-run is a no-op.
	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ObjectsDeleted)
}

func TestRemoveSystemImage_MalformedID(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: "vol-wrong"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDMalformed, err.Error())
}

func TestPreviewRemoveSystemImage_Salvage(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-salvage-preview"
	putAMIBlocks(t, store, id, 2, 4)

	preview, err := PreviewRemoveSystemImage(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.False(t, preview.ConfigPresent)
	assert.False(t, preview.ConfigCorrupt)
	assert.Equal(t, 2, preview.AMIObjectCount)
	assert.Equal(t, int64(8), preview.AMIBytesTotal)
}

func TestPreviewRemoveSystemImage_CorruptConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-corrupt-preview"
	putCorruptAMI(t, store, id)

	preview, err := PreviewRemoveSystemImage(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.False(t, preview.ConfigPresent)
	assert.True(t, preview.ConfigCorrupt)
}

func TestFindAMIDependents_SkipsTargetAMI(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-only"
	putAMI(t, store, id, "debian-13", "system", SnapPrefix(id))

	deps, err := FindAMIDependents(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.True(t, deps.Empty())
}
