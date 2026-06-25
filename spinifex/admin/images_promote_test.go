package admin

import (
	"bytes"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
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

	// Verify the persisted config now carries the system alias.
	meta, err := readAMIConfig(store, testRemoveBucket, id)
	require.NoError(t, err)
	assert.Equal(t, SystemOwnerAlias, meta.ImageOwnerAlias)
	// Other fields must be preserved.
	assert.Equal(t, "my-app", meta.Name)
	assert.Equal(t, "snap-user-001", meta.SnapshotID)
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
	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(id + "/config.json"),
		Body:   bytes.NewReader([]byte("{not valid json")),
	})
	require.NoError(t, err)

	_, err = PromoteSystemImage(store, testRemoveBucket, PromoteImageOpts{ImageID: id})
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
	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testRemoveBucket),
		Key:    aws.String(id + "/config.json"),
		Body:   bytes.NewReader([]byte("not json {")),
	})
	require.NoError(t, err)

	_, err = GetAMIMetadata(store, testRemoveBucket, id)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}
