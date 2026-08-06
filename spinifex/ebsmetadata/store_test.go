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
//
// A stub LegacyVolumeReader/LegacyAMIReader stands in for the real decoder in
// migrate/ebsmetadatabackfill (which this package must not import — that
// would be a cycle, since the real decoder imports ebsmetadata).

// stubLegacyVolumes backs a LegacyVolumeReader with a fixed id->Volume map,
// so tests can assert exactly which IDs the fallback is consulted for.
func stubLegacyVolumes(byID map[string]Volume) LegacyVolumeReader {
	return func(_ context.Context, _ objectstore.ObjectStore, _ string, volumeID string) (Volume, bool, error) {
		v, ok := byID[volumeID]
		return v, ok, nil
	}
}

func stubLegacyAMIs(byID map[string]AMI) LegacyAMIReader {
	return func(_ context.Context, _ objectstore.ObjectStore, _ string, imageID string) (AMI, bool, error) {
		a, ok := byID[imageID]
		return a, ok, nil
	}
}

// TestGetVolume_FallsBackWhenDocumentAbsent covers the read path fixing the
// invisibility bug: a volume with no ebsmetadata document is still readable
// via the legacy fallback.
func TestGetVolume_FallsBackWhenDocumentAbsent(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	store.SetLegacyVolumeFallback(stubLegacyVolumes(map[string]Volume{
		"vol-legacy": {VolumeID: "vol-legacy", CapacityGiB: 5, State: "available"},
	}))

	got, err := store.GetVolume(context.Background(), "vol-legacy")
	require.NoError(t, err)
	assert.Equal(t, "vol-legacy", got.VolumeID)
	assert.Equal(t, uint64(5), got.CapacityGiB)
}

// TestGetVolume_PrefersDocumentOverFallback covers precedence: once a
// document exists, it wins even though a (stale) fallback entry also exists.
func TestGetVolume_PrefersDocumentOverFallback(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	require.NoError(t, store.PutVolume(context.Background(), Volume{VolumeID: "vol-both", CapacityGiB: 99}))
	store.SetLegacyVolumeFallback(stubLegacyVolumes(map[string]Volume{
		"vol-both": {VolumeID: "vol-both", CapacityGiB: 1},
	}))

	got, err := store.GetVolume(context.Background(), "vol-both")
	require.NoError(t, err)
	assert.Equal(t, uint64(99), got.CapacityGiB, "the ebsmetadata document must win over the legacy fallback")
}

// TestGetVolume_NoFallbackConfigured_NotFoundUnchanged locks the "switched
// off" state: with no fallback wired, a missing document surfaces the
// original not-found error exactly as before this feature existed.
func TestGetVolume_NoFallbackConfigured_NotFoundUnchanged(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	_, err := store.GetVolume(context.Background(), "vol-missing")
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// TestGetVolume_FallbackMiss_ReturnsOriginalNotFound covers a volume ID that
// exists in neither the document store nor the legacy layout.
func TestGetVolume_FallbackMiss_ReturnsOriginalNotFound(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	store.SetLegacyVolumeFallback(stubLegacyVolumes(nil))

	_, err := store.GetVolume(context.Background(), "vol-nowhere")
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// TestGetAMI_FallsBackWhenDocumentAbsent mirrors TestGetVolume_FallsBackWhenDocumentAbsent.
func TestGetAMI_FallsBackWhenDocumentAbsent(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	store.SetLegacyAMIFallback(stubLegacyAMIs(map[string]AMI{
		"ami-legacy": {ImageID: "ami-legacy", Name: "legacy-image"},
	}))

	got, err := store.GetAMI(context.Background(), "ami-legacy")
	require.NoError(t, err)
	assert.Equal(t, "legacy-image", got.Name)
}

// TestGetAMI_PrefersDocumentOverFallback mirrors TestGetVolume_PrefersDocumentOverFallback.
func TestGetAMI_PrefersDocumentOverFallback(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	require.NoError(t, store.PutAMI(context.Background(), AMI{ImageID: "ami-both", Name: "document"}))
	store.SetLegacyAMIFallback(stubLegacyAMIs(map[string]AMI{
		"ami-both": {ImageID: "ami-both", Name: "legacy"},
	}))

	got, err := store.GetAMI(context.Background(), "ami-both")
	require.NoError(t, err)
	assert.Equal(t, "document", got.Name)
}

// TestListVolumes_UnionsAndDeduplicatesWithFallback is the Store-level
// assertion behind the DescribeVolumes invisibility fix: ListVolumes must
// enumerate legacy-only volumes, not just ebsmetadata documents, and prefer
// the document where both exist.
func TestListVolumes_UnionsAndDeduplicatesWithFallback(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-doc-only", CapacityGiB: 10}))
	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-both", CapacityGiB: 99}))
	// vol-legacy-only and vol-both are discoverable as legacy prefixes: seed a
	// marker object under each so legacyPrefixIDs' bucket-root scan finds them.
	seedLegacyPrefix(t, objects, "vol-legacy-only")
	seedLegacyPrefix(t, objects, "vol-both")
	seedLegacyPrefix(t, objects, "vol-legacy-only-efi") // must be excluded

	store.SetLegacyVolumeFallback(stubLegacyVolumes(map[string]Volume{
		"vol-legacy-only":     {VolumeID: "vol-legacy-only", CapacityGiB: 7},
		"vol-both":            {VolumeID: "vol-both", CapacityGiB: 1}, // must lose to the document
		"vol-legacy-only-efi": {VolumeID: "vol-legacy-only-efi", CapacityGiB: 1},
	}))

	got, err := store.ListVolumes(ctx)
	require.NoError(t, err)

	byID := make(map[string]Volume, len(got))
	for _, v := range got {
		byID[v.VolumeID] = v
	}
	require.Len(t, got, 3, "vol-doc-only, vol-both (deduplicated), vol-legacy-only; the -efi sub-volume must be excluded")
	assert.Equal(t, uint64(10), byID["vol-doc-only"].CapacityGiB)
	assert.Equal(t, uint64(99), byID["vol-both"].CapacityGiB, "the document must win over the legacy fallback")
	assert.Equal(t, uint64(7), byID["vol-legacy-only"].CapacityGiB)
	_, hasEFI := byID["vol-legacy-only-efi"]
	assert.False(t, hasEFI)
}

// TestListVolumes_NoFallbackConfigured_DocumentsOnlyUnchanged locks the
// "switched off" state: with no fallback wired, ListVolumes behaves exactly
// as it did before this feature existed.
func TestListVolumes_NoFallbackConfigured_DocumentsOnlyUnchanged(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-doc-only", CapacityGiB: 10}))
	seedLegacyPrefix(t, objects, "vol-legacy-only")

	got, err := store.ListVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "vol-doc-only", got[0].VolumeID)
}

// TestListAMIs_UnionsAndDeduplicatesWithFallback mirrors
// TestListVolumes_UnionsAndDeduplicatesWithFallback.
func TestListAMIs_UnionsAndDeduplicatesWithFallback(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutAMI(ctx, AMI{ImageID: "ami-doc-only", Name: "doc"}))
	require.NoError(t, store.PutAMI(ctx, AMI{ImageID: "ami-both", Name: "doc-wins"}))
	seedLegacyPrefix(t, objects, "ami-legacy-only")
	seedLegacyPrefix(t, objects, "ami-both")

	store.SetLegacyAMIFallback(stubLegacyAMIs(map[string]AMI{
		"ami-legacy-only": {ImageID: "ami-legacy-only", Name: "legacy"},
		"ami-both":        {ImageID: "ami-both", Name: "legacy-loses"},
	}))

	got, err := store.ListAMIs(ctx)
	require.NoError(t, err)

	byID := make(map[string]AMI, len(got))
	for _, a := range got {
		byID[a.ImageID] = a
	}
	require.Len(t, got, 3)
	assert.Equal(t, "doc", byID["ami-doc-only"].Name)
	assert.Equal(t, "doc-wins", byID["ami-both"].Name)
	assert.Equal(t, "legacy", byID["ami-legacy-only"].Name)
}

// seedLegacyPrefix writes a marker object so the id shows up as a top-level
// bucket prefix, the way a real vol-*/config.json or ami-*/config.json would.
func seedLegacyPrefix(t *testing.T, objects objectstore.ObjectStore, id string) {
	t.Helper()
	_, err := objects.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("control-plane"), Key: aws.String(id + "/config.json"), Body: bytes.NewReader([]byte("{}")),
	})
	require.NoError(t, err)
}
