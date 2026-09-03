package admin

import (
	"bytes"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPromoteSystemImage_HappyPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-user-001"
	putAMI(t, store, id, "my-app", testRemoveAccountID, "snap-user-001")

	result, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
	require.NoError(t, err)
	assert.Equal(t, testRemoveAccountID, result.PreviousOwner)

	// Verify the persisted document now carries the system alias.
	meta, err := readAMI(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.Equal(t, SystemOwnerAlias, meta.ImageOwnerAlias)
	// Other fields must be preserved.
	assert.Equal(t, "my-app", meta.Name)
	assert.Equal(t, "snap-user-001", meta.SnapshotID)
}

// A promoted AMI's readers derive the snapshot account from the alias, so the
// document has to end up under the global account or every launch of it falls
// back to the image ID and the provider refuses the clone.
func TestPromoteSystemImage_MovesSnapshotToGlobalAccount(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-user-snap"
	const snapID = "snap-user-snap"
	putAMI(t, store, id, "my-app", testRemoveAccountID, snapID)
	putSnapMetadata(t, store, snapID, "vol-source-001")

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
	require.NoError(t, err)

	metaStore := ebsmetadata.NewStore(store, testRemoveBucket)
	moved, err := metaStore.GetSnapshot(t.Context(), utils.GlobalAccountID, snapID)
	require.NoError(t, err)
	assert.Equal(t, utils.GlobalAccountID, moved.OwnerID)
	assert.Equal(t, "vol-source-001", moved.VolumeID)

	_, err = metaStore.GetSnapshot(t.Context(), testRemoveAccountID, snapID)
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err), "the superseded document must not survive: %v", err)
}

// A bundled image has no snapshot document; promotion still succeeds and the
// AMI keeps resolving through the fallback.
func TestPromoteSystemImage_NoSnapshotDocument_Promotes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-user-nosnap"
	putAMI(t, store, id, "bundled", testRemoveAccountID, "snap-absent")

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
	require.NoError(t, err)

	meta, err := readAMI(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.Equal(t, SystemOwnerAlias, meta.ImageOwnerAlias)
}

// Refusing beats promoting an AMI whose snapshot cannot be re-keyed, which
// would leave the alias pointing at a document no reader can find.
func TestPromoteSystemImage_CorruptSnapshotDocument_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-user-corruptsnap"
	const snapID = "snap-corrupt-001"
	putAMI(t, store, id, "my-app", testRemoveAccountID, snapID)
	key, err := ebsmetadata.SnapshotKey(testRemoveAccountID, snapID)
	require.NoError(t, err)
	_, err = store.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte("{not valid json")),
	})
	require.NoError(t, err)

	_, err = PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "corrupt")

	meta, err := readAMI(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.Equal(t, testRemoveAccountID, meta.ImageOwnerAlias, "the alias must not move without its snapshot")
}

func TestPromoteSystemImage_AlreadySystem_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-sys-001"
	putAMI(t, store, id, "debian-13", SystemOwnerAlias, "snap-sys-001")

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system-owned")
	assert.Contains(t, err.Error(), id)
}

func TestPromoteSystemImage_AlreadyOtherAlias_Refused(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-alias-001"
	// Any non-account owner is treated as already system-owned.
	putAMI(t, store, id, "other", "spinifex", "snap-alias-001")

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system-owned")
}

func TestPromoteSystemImage_MissingConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: "ami-missing"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestPromoteSystemImage_InvalidPrefix_Malformed(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: "snap-not-ami"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDMalformed, err.Error())
}

func TestPromoteSystemImage_CorruptConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-corrupt-promote"
	putCorruptAMI(t, store, id)

	_, err := PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestGetAMIMetadata_HappyPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-meta-001"
	putAMI(t, store, id, "ubuntu-24", testRemoveAccountID, "snap-meta-001")

	meta, err := GetAMIMetadata(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.Equal(t, "ubuntu-24", meta.Name)
	assert.Equal(t, testRemoveAccountID, meta.ImageOwnerAlias)
}

func TestGetAMIMetadata_MissingConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()

	_, err := GetAMIMetadata(store, testRemoveBucket, "ami-missing-meta")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestGetAMIMetadata_CorruptConfig_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const id = "ami-corrupt-meta"
	putCorruptAMI(t, store, id)

	_, err := GetAMIMetadata(store, testRemoveBucket, id)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}
