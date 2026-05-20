package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRemoveBucket    = "test-bucket"
	testRemoveAccountID = "000000000001"
)

// putAMI writes an ami-<id>/config.json with the given owner. Owner "" produces
// a corrupt config (empty ImageOwnerAlias is treated as corrupt by readers).
func putAMI(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name, owner, snapshotID string) {
	t.Helper()
	state := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         imageID,
				Name:            name,
				ImageOwnerAlias: owner,
				SnapshotID:      snapshotID,
				VolumeSizeGiB:   8,
			},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(imageID + "/config.json"),
		Body:   bytes.NewReader(data),
	})
	require.NoError(t, err)
}

// putAMIBlocks writes a few dummy chunk objects under ami-<id>/ so deletion
// has bulk work and byte counts to verify.
func putAMIBlocks(t *testing.T, store *objectstore.MemoryObjectStore, imageID string, n int, size int) {
	t.Helper()
	body := bytes.Repeat([]byte{0xab}, size)
	for i := range n {
		_, err := store.PutObject(&awss3.PutObjectInput{
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
		_, err := store.PutObject(&awss3.PutObjectInput{
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

// putVolume writes vol-<id>/config.json with the given SnapshotID reference.
func putVolume(t *testing.T, store *objectstore.MemoryObjectStore, volID, snapshotID string) {
	t.Helper()
	wrapper := volumeConfigWrapper{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:   volID,
				SnapshotID: snapshotID,
				SizeGiB:    8,
			},
		},
	}
	data, err := json.Marshal(wrapper)
	require.NoError(t, err)
	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(volID + "/config.json"),
		Body:   bytes.NewReader(data),
	})
	require.NoError(t, err)
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
	// config.json + 3 chunks = 4 ami-prefix objects.
	assert.Equal(t, 4, preview.AMIObjectCount)
	assert.Equal(t, 2, preview.SnapObjectCount)
	assert.True(t, preview.Dependents.Empty())

	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.NoError(t, err)
	assert.Equal(t, preview.AMIObjectCount+preview.SnapObjectCount, res.ObjectsDeleted)
	assert.Equal(t, preview.AMIBytesTotal+preview.SnapBytesTotal, res.BytesFreed)
	assert.Equal(t, 0, store.Count())
}

func TestRemoveSystemImage_AccountOwned_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-acct-001"
	putAMI(t, store, id, "user-ami", testRemoveAccountID, "snap-acct-001")
	putAMIBlocks(t, store, id, 1, 16)

	_, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account-owned")
	assert.Contains(t, err.Error(), "aws ec2 deregister-image")
	// Objects untouched.
	assert.Equal(t, 2, store.Count())
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
	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(id + "/config.json"),
		Body:   bytes.NewReader([]byte("{not valid json")),
	})
	require.NoError(t, err)

	_, err = RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
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
	require.True(t, errors.As(err, &depErr))
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
	require.True(t, errors.As(err, &depErr))
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
	require.True(t, errors.As(err, &depErr))
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
	assert.Greater(t, res.ObjectsDeleted, 0)
	// vol-orphan/config.json remains; the AMI is gone.
	_, err = store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(id + "/config.json"),
	})
	require.Error(t, err)
	_, err = store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String("vol-orphan/config.json"),
	})
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
	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(id + "/config.json"),
		Body:   bytes.NewReader([]byte("garbage")),
	})
	require.NoError(t, err)
	putAMIBlocks(t, store, id, 1, 8)

	// Without --force.
	_, err = RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// With --force: corrupt config + blocks all go.
	res, err := RemoveSystemImage(store, testRemoveBucket, RemoveImageOpts{ImageID: id, Force: true})
	require.NoError(t, err)
	assert.Equal(t, 2, res.ObjectsDeleted)
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
	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(id + "/config.json"),
		Body:   bytes.NewReader([]byte("{nope")),
	})
	require.NoError(t, err)

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
