package ebsprovider

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryProviderLifecycleAndIdempotency(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{CrashConsistentSnapshot: true})
	createdAt := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	provider.now = func() time.Time { return createdAt }
	ctx := context.Background()

	create := CreateVolumeRequest{
		Versioned:        NewVersioned(),
		VolumeID:         "vol-1",
		CapacityRange:    CapacityRange{RequiredBytes: 8 << 30},
		AvailabilityZone: "ap-southeast-2a",
	}
	volume, err := provider.CreateVolume(ctx, create)
	require.NoError(t, err)
	assert.Equal(t, "memory://volume/vol-1", volume.Handle)
	assert.Equal(t, VolumeStateAvailable, volume.State)

	repeated, err := provider.CreateVolume(ctx, create)
	require.NoError(t, err)
	assert.Equal(t, volume, repeated, "same-name/same-parameters create must be idempotent")

	changed := create
	changed.CapacityRange.RequiredBytes++
	_, err = provider.CreateVolume(ctx, changed)
	require.ErrorIs(t, err, ErrAlreadyExists)

	published, err := provider.PublishVolume(ctx, PublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "nbd+unix:///?socket=/memory/vol-1.sock", published.NBDURI)

	repeatedPublish, err := provider.PublishVolume(ctx, PublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-1",
	})
	require.NoError(t, err)
	assert.Equal(t, published, repeatedPublish)

	_, err = provider.PublishVolume(ctx, PublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-2",
	})
	require.ErrorIs(t, err, ErrVolumeInUse)
	require.ErrorIs(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
	}), ErrVolumeInUse)

	require.NoError(t, provider.UnpublishVolume(ctx, UnpublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-1",
	}))

	expanded, err := provider.ExpandVolume(ctx, ExpandVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
		CapacityRange: CapacityRange{RequiredBytes: 16 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(16<<30), expanded.CapacityBytes)

	snapshot, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: "snap-1", VolumeID: volume.ID, VolumeHandle: volume.Handle,
	})
	require.NoError(t, err)
	assert.Equal(t, createdAt, snapshot.CreatedAt)
	assert.Equal(t, SnapshotStateCompleted, snapshot.State)
	assert.Equal(t, expanded.CapacityBytes, snapshot.SizeBytes)

	repeatedSnapshot, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: "snap-1", VolumeID: volume.ID, VolumeHandle: volume.Handle,
	})
	require.NoError(t, err)
	assert.Equal(t, snapshot, repeatedSnapshot)

	require.NoError(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
	}))
	require.NoError(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
	}), "delete of an absent volume must be idempotent")

	require.NoError(t, provider.DeleteSnapshot(ctx, DeleteSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: snapshot.ID, Handle: snapshot.Handle,
	}))
	require.NoError(t, provider.DeleteSnapshot(ctx, DeleteSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: snapshot.ID, Handle: snapshot.Handle,
	}), "delete of an absent snapshot must be idempotent")
}

func TestMemoryProviderRequiresExplicitSchemaVersion(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
		VolumeID: "vol-unversioned", CapacityRange: CapacityRange{RequiredBytes: 1},
	})
	require.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestProviderErrorUnwrap(t *testing.T) {
	err := &ProviderError{Code: ErrorCodeNotFound, Message: "volume disappeared"}
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, "volume disappeared", err.Error())
}

// TestMemoryProviderChecksVersionOnEveryMethod covers the checkVersion guard
// on every one of the nine EBSProvider methods directly on MemoryProvider,
// independent of NATSProvider's own client-side pre-check.
func TestMemoryProviderChecksVersionOnEveryMethod(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	ctx := context.Background()

	_, err := provider.GetCapabilities(ctx, GetCapabilitiesRequest{})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	_, err = provider.CreateVolume(ctx, CreateVolumeRequest{VolumeID: "vol-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	_, err = provider.GetVolume(ctx, GetVolumeRequest{VolumeID: "vol-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	_, err = provider.ExpandVolume(ctx, ExpandVolumeRequest{VolumeID: "vol-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	err = provider.DeleteVolume(ctx, DeleteVolumeRequest{VolumeID: "vol-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	_, err = provider.CreateSnapshot(ctx, CreateSnapshotRequest{SnapshotID: "snap-1", VolumeID: "vol-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	err = provider.DeleteSnapshot(ctx, DeleteSnapshotRequest{SnapshotID: "snap-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	_, err = provider.PublishVolume(ctx, PublishVolumeRequest{VolumeID: "vol-1", NodeID: "node-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	err = provider.UnpublishVolume(ctx, UnpublishVolumeRequest{VolumeID: "vol-1", NodeID: "node-1"})
	require.ErrorIs(t, err, ErrUnsupportedVersion)
}

// TestMemoryProviderCreateVolume_SourceSnapshotTooSmall covers CreateVolume's
// snapshot-capacity guard: a volume created from a snapshot must be at least
// as large as the snapshot it is cloned from.
func TestMemoryProviderCreateVolume_SourceSnapshotTooSmall(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	ctx := context.Background()

	_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-src", CapacityRange: CapacityRange{RequiredBytes: 4 << 30}})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-src", VolumeID: "vol-src"})
	require.NoError(t, err)

	_, err = provider.CreateVolume(ctx, CreateVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-too-small", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}, SourceSnapshotID: "snap-src",
	})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

// TestMemoryProviderExpandVolume_InvalidArgument covers ExpandVolume's own
// argument validation (empty volume ID, non-positive capacity), separate
// from the not_found and grow-only cases covered elsewhere.
func TestMemoryProviderExpandVolume_InvalidArgument(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	_, err := provider.ExpandVolume(context.Background(), ExpandVolumeRequest{Versioned: NewVersioned()})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

// TestMemoryProviderExpandVolume_HandleMismatchIsNotFound covers the handle
// mismatch arm of ExpandVolume's not_found check: a stale/wrong handle must
// be treated the same as a missing volume, not silently ignored.
func TestMemoryProviderExpandVolume_HandleMismatchIsNotFound(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	ctx := context.Background()
	_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-handle", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
	require.NoError(t, err)

	_, err = provider.ExpandVolume(ctx, ExpandVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-handle", Handle: "memory://volume/stale", CapacityRange: CapacityRange{RequiredBytes: 2 << 30},
	})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestMemoryProviderDeleteVolume_InvalidArgumentAndHandleMismatch covers
// DeleteVolume's empty-ID rejection and its handle-mismatch no-op: a caller
// deleting by a stale handle must not delete the current volume out from
// under a newer handle.
func TestMemoryProviderDeleteVolume_InvalidArgumentAndHandleMismatch(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	ctx := context.Background()

	err := provider.DeleteVolume(ctx, DeleteVolumeRequest{Versioned: NewVersioned()})
	require.ErrorIs(t, err, ErrInvalidArgument)

	_, createErr := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-stale-handle", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
	require.NoError(t, createErr)
	require.NoError(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-stale-handle", Handle: "memory://volume/wrong"}))

	// The volume must still exist: the mismatched-handle delete above must
	// have been a no-op, not a real deletion.
	got, err := provider.GetVolume(ctx, GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-stale-handle"})
	require.NoError(t, err)
	assert.Equal(t, "vol-stale-handle", got.ID)
}

// TestMemoryProviderDeleteSnapshot_InvalidArgument covers DeleteSnapshot's
// empty-ID rejection, the one argument-validation arm no other test reaches.
func TestMemoryProviderDeleteSnapshot_InvalidArgument(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	err := provider.DeleteSnapshot(context.Background(), DeleteSnapshotRequest{Versioned: NewVersioned()})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

// TestCloneHelpersHandleNil covers the nil-input arm of each clone helper:
// none of MemoryProvider's current call sites pass a nil pointer, so this
// arm is otherwise unreachable dead code from the method-level tests alone.
func TestCloneHelpersHandleNil(t *testing.T) {
	assert.Nil(t, cloneVolume(nil))
	assert.Nil(t, cloneSnapshot(nil))
	assert.Nil(t, clonePublished(nil))
}

// A provider that does not advertise VolumeSeeding must refuse a seed outright
// rather than accepting the request and silently dropping the bytes, which
// would leave a guest booting an EFI volume full of zeroes.
func TestMemoryProviderSeedRequiresCapability(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-seed-nocap",
		CapacityRange: CapacityRange{RequiredBytes: 4096},
		SeedData:      bytes.Repeat([]byte{0x01}, 16),
	})
	require.ErrorIs(t, err, ErrUnsupportedCapability)
}

func TestMemoryProviderSeedIsStoredOnce(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{VolumeSeeding: true})
	ctx := context.Background()
	seed := bytes.Repeat([]byte{0xAB}, 512)

	create := CreateVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-seed-once",
		CapacityRange: CapacityRange{RequiredBytes: 4096}, SeedData: seed,
	}
	_, err := provider.CreateVolume(ctx, create)
	require.NoError(t, err)

	stored, ok := provider.SeedData("vol-seed-once")
	require.True(t, ok)
	assert.Equal(t, seed, stored)

	// A relaunch reissues the same create. Reseeding here would overwrite an
	// EFI variable store the guest has since written its own BootOrder into.
	reseed := create
	reseed.SeedData = bytes.Repeat([]byte{0xCD}, 512)
	_, err = provider.CreateVolume(ctx, reseed)
	require.NoError(t, err, "a repeat create must stay idempotent regardless of seed")

	stored, ok = provider.SeedData("vol-seed-once")
	require.True(t, ok)
	assert.Equal(t, seed, stored, "an existing volume must never be reseeded")
}

// TestMemoryProviderEnumerationRequiresCapability pins the refusal a caller
// branches on: a provider that cannot enumerate must say so, because an empty
// page and "I do not enumerate" mean opposite things to a reconciler deciding
// whether volumes with no metadata document have been orphaned.
func TestMemoryProviderEnumerationRequiresCapability(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	ctx := context.Background()

	_, err := provider.ListVolumes(ctx, ListVolumesRequest{Versioned: NewVersioned()})
	require.ErrorIs(t, err, ErrUnsupportedCapability)

	_, err = provider.ListSnapshots(ctx, ListSnapshotsRequest{Versioned: NewVersioned()})
	require.ErrorIs(t, err, ErrUnsupportedCapability)
}

// TestMemoryProviderListVolumesPages walks a full enumeration one page at a
// time. The token is the ID to resume after, so the property worth pinning is
// that concatenating the pages yields every volume exactly once, in ID order,
// with the last page carrying no token.
func TestMemoryProviderListVolumesPages(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{VolumeEnumeration: true})
	ctx := context.Background()

	empty, err := provider.ListVolumes(ctx, ListVolumesRequest{Versioned: NewVersioned()})
	require.NoError(t, err)
	assert.Empty(t, empty.Volumes)
	assert.Empty(t, empty.NextToken, "holding nothing is a last page, not a page to resume")

	for _, id := range []string{"vol-c", "vol-a", "vol-e", "vol-b", "vol-d"} {
		_, err := provider.CreateVolume(ctx, CreateVolumeRequest{
			Versioned: NewVersioned(), VolumeID: id, CapacityRange: CapacityRange{RequiredBytes: 1 << 30},
		})
		require.NoError(t, err)
	}

	var seen []VolumeRef
	var token string
	for range 3 {
		page, err := provider.ListVolumes(ctx, ListVolumesRequest{
			Versioned: NewVersioned(), MaxResults: 2, StartingToken: token,
		})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Volumes), 2, "a page must not exceed MaxResults")
		seen = append(seen, page.Volumes...)
		token = page.NextToken
		if token == "" {
			break
		}
	}

	assert.Empty(t, token, "the walk must terminate")
	assert.Equal(t, []VolumeRef{
		{ID: "vol-a", Handle: "memory://volume/vol-a"},
		{ID: "vol-b", Handle: "memory://volume/vol-b"},
		{ID: "vol-c", Handle: "memory://volume/vol-c"},
		{ID: "vol-d", Handle: "memory://volume/vol-d"},
		{ID: "vol-e", Handle: "memory://volume/vol-e"},
	}, seen)
}

// TestMemoryProviderListSnapshotsCarriesSourceVolume covers the one field that
// makes snapshot enumeration actionable: a snapshot found with no metadata
// document can only be reconciled once the volume it came from is known.
func TestMemoryProviderListSnapshotsCarriesSourceVolume(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{SnapshotEnumeration: true})
	ctx := context.Background()

	_, err := provider.CreateVolume(ctx, CreateVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-src", CapacityRange: CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	for _, id := range []string{"snap-b", "snap-a"} {
		_, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{
			Versioned: NewVersioned(), SnapshotID: id, VolumeID: "vol-src",
		})
		require.NoError(t, err)
	}

	first, err := provider.ListSnapshots(ctx, ListSnapshotsRequest{Versioned: NewVersioned(), MaxResults: 1})
	require.NoError(t, err)
	assert.Equal(t, []SnapshotRef{{ID: "snap-a", SourceVolumeID: "vol-src", Handle: "memory://snapshot/snap-a"}}, first.Snapshots)
	require.Equal(t, "snap-a", first.NextToken)

	second, err := provider.ListSnapshots(ctx, ListSnapshotsRequest{
		Versioned: NewVersioned(), MaxResults: 1, StartingToken: first.NextToken,
	})
	require.NoError(t, err)
	assert.Equal(t, []SnapshotRef{{ID: "snap-b", SourceVolumeID: "vol-src", Handle: "memory://snapshot/snap-b"}}, second.Snapshots)
	assert.Empty(t, second.NextToken)
}

// TestMemoryProviderCopySnapshot covers the duplication contract: the copy is
// an independent snapshot over the same frozen data, and every refusal is a
// case where copying would either lose data or attribute it to the wrong
// volume.
func TestMemoryProviderCopySnapshot(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	copiedAt := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	provider.now = func() time.Time { return copiedAt }
	ctx := context.Background()

	_, err := provider.CreateVolume(ctx, CreateVolumeRequest{
		Versioned: NewVersioned(), VolumeID: "vol-src", CapacityRange: CapacityRange{RequiredBytes: 4 << 30},
	})
	require.NoError(t, err)
	source, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: "snap-src", VolumeID: "vol-src",
	})
	require.NoError(t, err)

	copied, err := provider.CopySnapshot(ctx, CopySnapshotRequest{
		Versioned: NewVersioned(), SourceSnapshotID: "snap-src", DestinationSnapshotID: "snap-dst", VolumeID: "vol-src",
	})
	require.NoError(t, err)
	assert.Equal(t, "snap-dst", copied.ID)
	assert.Equal(t, "memory://snapshot/snap-dst", copied.Handle, "the copy must be independently addressable")
	assert.Equal(t, source.SourceVolumeID, copied.SourceVolumeID)
	assert.Equal(t, source.SizeBytes, copied.SizeBytes)
	assert.Equal(t, copiedAt, copied.CreatedAt)
	assert.Equal(t, SnapshotStateCompleted, copied.State)

	stored, ok := provider.Snapshot("snap-dst")
	require.True(t, ok, "the copy must reach the provider, not just the response")
	assert.Equal(t, copied, stored)
	_, ok = provider.Snapshot("snap-absent")
	assert.False(t, ok)

	refusals := []struct {
		name string
		req  CopySnapshotRequest
		want error
	}{
		{"missing source", CopySnapshotRequest{DestinationSnapshotID: "snap-x", VolumeID: "vol-src"}, ErrInvalidArgument},
		{"missing destination", CopySnapshotRequest{SourceSnapshotID: "snap-src", VolumeID: "vol-src"}, ErrInvalidArgument},
		{"missing volume", CopySnapshotRequest{SourceSnapshotID: "snap-src", DestinationSnapshotID: "snap-x"}, ErrInvalidArgument},
		{"destination equals source", CopySnapshotRequest{SourceSnapshotID: "snap-src", DestinationSnapshotID: "snap-src", VolumeID: "vol-src"}, ErrInvalidArgument},
		{"unknown source", CopySnapshotRequest{SourceSnapshotID: "snap-absent", DestinationSnapshotID: "snap-x", VolumeID: "vol-src"}, ErrNotFound},
		{"wrong source volume", CopySnapshotRequest{SourceSnapshotID: "snap-src", DestinationSnapshotID: "snap-x", VolumeID: "vol-other"}, ErrInvalidArgument},
		{"destination exists", CopySnapshotRequest{SourceSnapshotID: "snap-src", DestinationSnapshotID: "snap-dst", VolumeID: "vol-src"}, ErrAlreadyExists},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			req.Versioned = NewVersioned()
			_, err := provider.CopySnapshot(ctx, req)
			require.ErrorIs(t, err, tt.want)
		})
	}

	t.Run("version skew", func(t *testing.T) {
		_, err := provider.CopySnapshot(ctx, CopySnapshotRequest{
			SourceSnapshotID: "snap-src", DestinationSnapshotID: "snap-y", VolumeID: "vol-src",
		})
		require.ErrorIs(t, err, ErrUnsupportedVersion)
	})
}

func TestValidateSeedData(t *testing.T) {
	require.NoError(t, ValidateSeedData(nil))
	require.NoError(t, ValidateSeedData(make([]byte, MaxSeedBytes)))
	require.ErrorIs(t, ValidateSeedData(make([]byte, MaxSeedBytes+1)), ErrInvalidArgument)
}
