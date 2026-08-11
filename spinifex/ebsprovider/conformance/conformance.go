// Package conformance exercises the ebsprovider.EBSProvider contract
// itself, not any one implementation's internals, so it can run unmodified
// against every implementation of the interface; no assertion here has changed from the original suite it was moved out of.
package conformance

import (
	"bytes"
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ReferenceCapabilities is what a full-featured provider advertises. It is
// only a convenience for callers constructing a MemoryProvider; the suite
// itself never assumes any implementation reports this set.
var ReferenceCapabilities = ebsprovider.Capabilities{
	CopyOnWriteClone:        true,
	OnlineExpansion:         false,
	SparseExtentReporting:   true,
	CrashConsistentSnapshot: true,
	VolumeSeeding:           true,
	ReadOnlyPublish:         true,
}

// capabilitiesOf reads what a provider advertises, so the suite can branch on
// it the way the contract tells callers to.
func capabilitiesOf(t *testing.T, provider ebsprovider.EBSProvider) ebsprovider.Capabilities {
	t.Helper()
	resp, err := provider.GetCapabilities(context.Background(), ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp.Capabilities
}

// RunSuite runs the full EBSProvider conformance suite against a provider
// newProvider constructs fresh for each subtest. Optional behaviour is gated
// on what the provider advertises, never on a set the suite picked.
func RunSuite(t *testing.T, newProvider func(t *testing.T) ebsprovider.EBSProvider) {
	capabilities := capabilitiesOf(t, newProvider(t))

	t.Run("GetCapabilities", func(t *testing.T) {
		provider := newProvider(t)
		resp, err := provider.GetCapabilities(context.Background(), ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, ebsprovider.SchemaVersion, resp.SchemaVersion, "response must carry the negotiated schema version")

		// Capabilities describe the implementation, so they must not vary
		// between calls: a caller branches on them once and caches.
		again, err := provider.GetCapabilities(context.Background(), ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
		require.NoError(t, err)
		assert.Equal(t, resp.Capabilities, again.Capabilities, "GetCapabilities must be stable across calls")
	})

	t.Run("CreateVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			vol, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-create-ok", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
			})
			require.NoError(t, err)
			require.NotNil(t, vol)
			assert.Equal(t, "vol-create-ok", vol.ID)
			assert.Equal(t, int64(1<<30), vol.CapacityBytes)
			assert.Equal(t, ebsprovider.VolumeStateAvailable, vol.State)
			assert.NotEmpty(t, vol.Handle)
		})

		t.Run("already_exists on conflicting recreate", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-conflict", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-conflict", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
			require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists)
		})

		t.Run("invalid_argument on empty volume id", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("not_found on absent source snapshot", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-from-missing-snap",
				CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
				SourceSnapshotID: "snap-missing", SourceSnapshotVolumeID: "vol-missing-origin",
			})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		// SourceSnapshotVolumeID names the volume the snapshot came from, which
		// a provider may need to resolve the snapshot's blocks. Omitting it is a
		// malformed request, not a request for behaviour the provider lacks.
		t.Run("invalid_argument when a source snapshot has no source volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-snapsrc-noorigin",
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}, SourceSnapshotID: "snap-any",
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("volume is created from a snapshot", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-snapsrc-origin",
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
			})
			require.NoError(t, err)
			snap, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-snapsrc", VolumeID: "vol-snapsrc-origin",
			})
			require.NoError(t, err)
			require.Equal(t, "vol-snapsrc-origin", snap.SourceVolumeID, "a snapshot must report the volume it was taken from")

			restored, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-snapsrc-restored",
				CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
				SourceSnapshotID: snap.ID, SourceSnapshotVolumeID: snap.SourceVolumeID,
			})
			require.NoError(t, err)
			assert.Equal(t, ebsprovider.VolumeStateAvailable, restored.State)
		})

		t.Run("unsupported_version", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				VolumeID: "vol-noversion", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
			})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)
		})

		t.Run("seeded volume is created", func(t *testing.T) {
			if !capabilities.VolumeSeeding {
				t.Skip("provider does not advertise VolumeSeeding")
			}
			provider := newProvider(t)
			vol, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-seeded",
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4096},
				SeedData:      bytes.Repeat([]byte{0xAB}, 4096),
			})
			require.NoError(t, err)
			require.NotNil(t, vol)
			assert.Equal(t, int64(4096), vol.CapacityBytes)
		})

		// A provider that cannot seed must say so, not accept the seed and
		// drop it: the caller has no other way to learn the volume is blank.
		t.Run("unsupported_capability on seed when seeding is not advertised", func(t *testing.T) {
			if capabilities.VolumeSeeding {
				t.Skip("provider advertises VolumeSeeding")
			}
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-seed-unsupported",
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4096},
				SeedData:      bytes.Repeat([]byte{0xAB}, 4096),
			})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedCapability)
		})

		// A seed above MaxSeedBytes must fail as invalid_argument rather than
		// reaching the transport, where NATS would refuse the oversized publish
		// with an error that says nothing about firmware.
		t.Run("invalid_argument on oversized seed", func(t *testing.T) {
			if !capabilities.VolumeSeeding {
				t.Skip("provider does not advertise VolumeSeeding")
			}
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-seed-toobig",
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
				SeedData:      make([]byte, ebsprovider.MaxSeedBytes+1),
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("invalid_argument when seed exceeds capacity", func(t *testing.T) {
			if !capabilities.VolumeSeeding {
				t.Skip("provider does not advertise VolumeSeeding")
			}
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-seed-overcap",
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 512},
				SeedData:      make([]byte, 4096),
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})
	})

	t.Run("GetVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			created, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-get-ok", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			got, err := provider.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-get-ok"})
			require.NoError(t, err)
			assert.Equal(t, created.Handle, got.Handle)
			assert.Equal(t, created.CapacityBytes, got.CapacityBytes)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-never-existed"})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("invalid_argument on empty volume id", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})
	})

	t.Run("ExpandVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-expand-ok", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			expanded, err := provider.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-expand-ok", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
			require.NoError(t, err)
			assert.Equal(t, int64(2<<30), expanded.CapacityBytes)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.ExpandVolume(context.Background(), ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-never-existed", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("invalid_argument on shrink", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-shrink", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
			require.NoError(t, err)
			_, err = provider.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-shrink", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		// Expanding a published volume is the one operation OnlineExpansion
		// describes, so both answers are contractual: succeed, or refuse with
		// volume_in_use. Silently doing nothing is neither.
		t.Run("expanding a published volume matches OnlineExpansion", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-expand-inuse", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-expand-inuse", NodeID: "node-1"})
			require.NoError(t, err)

			expanded, err := provider.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-expand-inuse", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
			if !capabilities.OnlineExpansion {
				require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
				return
			}
			require.NoError(t, err, "provider advertises OnlineExpansion, so expanding a published volume must succeed")
			require.NotNil(t, expanded)
			assert.Equal(t, int64(2<<30), expanded.CapacityBytes)
		})
	})

	t.Run("DeleteVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-delete-ok", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			require.NoError(t, provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-delete-ok"}))
			_, err = provider.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-delete-ok"})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		// CSI's controller service treats DeleteVolume on an absent volume as
		// success (idempotent-when-absent), not not_found.
		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-never-existed"}))
		})

		t.Run("volume_in_use when published", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-delete-inuse", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-delete-inuse", NodeID: "node-1"})
			require.NoError(t, err)
			err = provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-delete-inuse"})
			require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
		})
	})

	t.Run("CreateSnapshot", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-snap-src", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			snap, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-ok", VolumeID: "vol-snap-src"})
			require.NoError(t, err)
			require.NotNil(t, snap)
			assert.Equal(t, "snap-ok", snap.ID)
			assert.Equal(t, "vol-snap-src", snap.SourceVolumeID)
			assert.Equal(t, ebsprovider.SnapshotStateCompleted, snap.State)
		})

		t.Run("not_found on absent source volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-orphan", VolumeID: "vol-never-existed"})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("already_exists on conflicting source volume", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-snap-a", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-snap-b", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-conflict", VolumeID: "vol-snap-a"})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-conflict", VolumeID: "vol-snap-b"})
			require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists)
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})
	})

	t.Run("DeleteSnapshot", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-delsnap-src", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-delete-ok", VolumeID: "vol-delsnap-src"})
			require.NoError(t, err)
			require.NoError(t, provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-delete-ok"}))
		})

		// memory.go implements delete-of-absent-snapshot as a no-op success,
		// matching CSI's idempotency rule the same way DeleteVolume does.
		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-never-existed"}))
		})
	})

	t.Run("CopySnapshot", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-copysnap-src", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copysnap-src", VolumeID: "vol-copysnap-src"})
			require.NoError(t, err)

			copied, err := provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: "snap-copysnap-src", DestinationSnapshotID: "snap-copysnap-dst", VolumeID: "vol-copysnap-src",
			})
			require.NoError(t, err)
			require.NotNil(t, copied)
			assert.Equal(t, "snap-copysnap-dst", copied.ID)
			assert.Equal(t, "vol-copysnap-src", copied.SourceVolumeID)
			assert.Equal(t, ebsprovider.SnapshotStateCompleted, copied.State)
			assert.NotEmpty(t, copied.Handle)

			// The destination is a real, independently addressable snapshot.
			require.NoError(t, provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copysnap-src"}))
			require.NoError(t, provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copysnap-dst"}))
		})

		t.Run("not_found on absent source snapshot", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-copysnap-nosrc", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: "snap-copysnap-missing", DestinationSnapshotID: "snap-copysnap-missing-dst", VolumeID: "vol-copysnap-nosrc",
			})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("already_exists on conflicting destination", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-copysnap-conflict", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copysnap-a", VolumeID: "vol-copysnap-conflict"})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copysnap-b", VolumeID: "vol-copysnap-conflict"})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: "snap-copysnap-a", DestinationSnapshotID: "snap-copysnap-b", VolumeID: "vol-copysnap-conflict",
			})
			require.ErrorIs(t, err, ebsprovider.ErrAlreadyExists)
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("invalid_argument when source and destination match", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-copysnap-same", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copysnap-same", VolumeID: "vol-copysnap-same"})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: "snap-copysnap-same", DestinationSnapshotID: "snap-copysnap-same", VolumeID: "vol-copysnap-same",
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("invalid_argument when volume id does not own the source snapshot", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-copysnap-owner", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-copysnap-foreign", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-copysnap-owned", VolumeID: "vol-copysnap-owner"})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: "snap-copysnap-owned", DestinationSnapshotID: "snap-copysnap-owned-dst", VolumeID: "vol-copysnap-foreign",
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("unsupported_version", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
				SourceSnapshotID: "snap-a", DestinationSnapshotID: "snap-b", VolumeID: "vol-a",
			})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)
		})
	})

	t.Run("PublishVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-ok", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			pub, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-ok", NodeID: "node-1"})
			require.NoError(t, err)
			require.NotNil(t, pub)
			assert.Equal(t, "vol-pub-ok", pub.VolumeID)
			assert.Equal(t, "node-1", pub.NodeID)
			assertNBDURI(t, pub.NBDURI)
		})

		t.Run("idempotent republish to the same node", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-idem", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			first, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-idem", NodeID: "node-1"})
			require.NoError(t, err)
			second, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-idem", NodeID: "node-1"})
			require.NoError(t, err)
			assert.Equal(t, first, second)
		})

		t.Run("volume_in_use when published to a different node", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-conflict", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-conflict", NodeID: "node-1"})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-pub-conflict", NodeID: "node-2"})
			require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-never-existed", NodeID: "node-1"})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})
	})

	t.Run("UnpublishVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-unpub-ok", CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-unpub-ok", NodeID: "node-1"})
			require.NoError(t, err)
			require.NoError(t, provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-unpub-ok", NodeID: "node-1"}))
		})

		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-never-existed", NodeID: "node-1"}))
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			err := provider.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})
	})
}
