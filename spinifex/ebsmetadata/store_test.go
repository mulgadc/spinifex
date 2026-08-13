package ebsmetadata

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreRoundTripsVolumeAndAMI(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")

	volume := Volume{VolumeID: "vol-1", TenantID: "acct-1", CapacityGiB: 10, State: "available", ProviderHandle: "opaque"}
	require.NoError(t, store.PutVolume(context.Background(), volume))
	gotVolume, err := store.GetVolume(context.Background(), volume.VolumeID)
	require.NoError(t, err)
	assert.Equal(t, volume.VolumeID, gotVolume.VolumeID)
	assert.Equal(t, volume.ProviderHandle, gotVolume.ProviderHandle)
	assert.Equal(t, SchemaVersion, gotVolume.SchemaVersion)

	ami := AMI{ImageID: "ami-1", Name: "test", SnapshotID: "snap-1"}
	require.NoError(t, store.PutAMI(context.Background(), ami))
	gotAMI, err := store.GetAMI(context.Background(), ami.ImageID)
	require.NoError(t, err)
	assert.Equal(t, ami.ImageID, gotAMI.ImageID)
	assert.Equal(t, ami.SnapshotID, gotAMI.SnapshotID)
	assert.Equal(t, SchemaVersion, gotAMI.SchemaVersion)

	require.NoError(t, store.DeleteVolume(context.Background(), volume.VolumeID))
	require.NoError(t, store.DeleteAMI(context.Background(), ami.ImageID))
}

func TestListVolumes_Empty(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	volumes, err := store.ListVolumes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, volumes)
}

func TestListVolumes_ReturnsAllStoredVolumes(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()

	want := []Volume{
		{VolumeID: "vol-1", TenantID: "acct-1", CapacityGiB: 8, State: "available"},
		{VolumeID: "vol-2", TenantID: "acct-2", CapacityGiB: 16, State: "in-use"},
		{VolumeID: "vol-3", TenantID: "acct-1", CapacityGiB: 32, State: "available"},
	}
	for _, v := range want {
		require.NoError(t, store.PutVolume(ctx, v))
	}

	got, err := store.ListVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, got, len(want))

	gotIDs := make(map[string]uint64, len(got))
	for _, v := range got {
		gotIDs[v.VolumeID] = v.CapacityGiB
	}
	for _, v := range want {
		assert.Equal(t, v.CapacityGiB, gotIDs[v.VolumeID])
	}
}

// TestListVolumes_CorruptObjectReturnsError covers the UnmarshalVolume error
// path: an object under the volumes prefix that isn't a valid Volume record
// must fail the whole listing rather than being silently skipped.
func TestListVolumes_CorruptObjectReturnsError(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-good", CapacityGiB: 1}))

	key, err := VolumeKey("vol-corrupt")
	require.NoError(t, err)
	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("control-plane"), Key: aws.String(key), Body: bytes.NewReader([]byte("not json")),
	})
	require.NoError(t, err)

	_, err = store.ListVolumes(ctx)
	require.Error(t, err)
}

// TestListVolumes_NotConfigured covers the nil-store guard shared by every
// Store method: a zero-value *Store (no ObjectStore wired up) must error
// instead of panicking on a nil dereference.
func TestListVolumes_NotConfigured(t *testing.T) {
	var store *Store
	_, err := store.ListVolumes(context.Background())
	require.Error(t, err)

	empty := &Store{}
	_, err = empty.ListVolumes(context.Background())
	require.Error(t, err)
}

// TestListAMIs_Empty mirrors TestListVolumes_Empty: an AMI-less store lists
// as an empty slice, not an error.
func TestListAMIs_Empty(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	amis, err := store.ListAMIs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, amis)
}

// TestListAMIs_ReturnsAllStoredAMIs mirrors TestListVolumes_ReturnsAllStoredVolumes.
func TestListAMIs_ReturnsAllStoredAMIs(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()

	want := []AMI{
		{ImageID: "ami-1", Name: "one", VolumeSizeGiB: 8},
		{ImageID: "ami-2", Name: "two", VolumeSizeGiB: 16},
	}
	for _, a := range want {
		require.NoError(t, store.PutAMI(ctx, a))
	}

	got, err := store.ListAMIs(ctx)
	require.NoError(t, err)
	require.Len(t, got, len(want))

	gotSizes := make(map[string]uint64, len(got))
	for _, a := range got {
		gotSizes[a.ImageID] = a.VolumeSizeGiB
	}
	for _, a := range want {
		assert.Equal(t, a.VolumeSizeGiB, gotSizes[a.ImageID])
	}
}

// TestListAMIs_NotConfigured mirrors TestListVolumes_NotConfigured.
func TestListAMIs_NotConfigured(t *testing.T) {
	var store *Store
	_, err := store.ListAMIs(context.Background())
	require.Error(t, err)

	empty := &Store{}
	_, err = empty.ListAMIs(context.Background())
	require.Error(t, err)
}

// --- Legacy fallback tests ---
// TestGetVolume_MissingDocumentIsNotFound locks the only answer a volume with
// no document gets: it does not exist as far as the control plane is
// concerned, reported as the object store's not-found rather than a zero value.
func TestGetVolume_MissingDocumentIsNotFound(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	_, err := store.GetVolume(context.Background(), "vol-missing")
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// TestGetAMI_MissingDocumentIsNotFound mirrors TestGetVolume_MissingDocumentIsNotFound.
func TestGetAMI_MissingDocumentIsNotFound(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	_, err := store.GetAMI(context.Background(), "ami-missing")
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// TestGet_CorruptDocumentIsDistinguishable is what lets the admin tooling tell
// salvage from not-found: an undecodable document must not read as absent, or
// a --force removal would refuse the one case it exists for.
func TestGet_CorruptDocumentIsDistinguishable(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	volKey, err := VolumeKey("vol-corrupt")
	require.NoError(t, err)
	writeRaw(t, objects, volKey, []byte("{not json"))
	amiKey, err := AMIKey("ami-corrupt")
	require.NoError(t, err)
	writeRaw(t, objects, amiKey, []byte("{not json"))

	_, err = store.GetVolume(ctx, "vol-corrupt")
	require.ErrorIs(t, err, ErrCorruptDocument)
	assert.False(t, objectstore.IsNoSuchKeyError(err), "corrupt must not read as absent")

	_, err = store.GetAMI(ctx, "ami-corrupt")
	require.ErrorIs(t, err, ErrCorruptDocument)
	assert.False(t, objectstore.IsNoSuchKeyError(err), "corrupt must not read as absent")
}

// writeRaw puts bytes at key without going through Marshal, so a test can
// store a document the store cannot decode.
func writeRaw(t *testing.T, objects objectstore.ObjectStore, key string, data []byte) {
	t.Helper()
	_, err := objects.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("control-plane"), Key: aws.String(key), Body: bytes.NewReader(data),
	})
	require.NoError(t, err)
}

// TestListAMIs_SkipsCorruptButStrictDoesNot pins the blast radius of one bad
// document: DescribeImages keeps working and loses only the unreadable image,
// while a caller that cannot answer partially still gets the error.
func TestListAMIs_SkipsCorruptButStrictDoesNot(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutAMI(ctx, AMI{ImageID: "ami-good", Name: "readable"}))
	badKey, err := AMIKey("ami-bad")
	require.NoError(t, err)
	writeRaw(t, objects, badKey, []byte("{not json"))

	got, err := store.ListAMIs(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1, "the readable AMI must survive its neighbour being corrupt")
	assert.Equal(t, "ami-good", got[0].ImageID)

	_, err = store.ListAMIsStrict(ctx)
	require.ErrorIs(t, err, ErrCorruptDocument)
}
