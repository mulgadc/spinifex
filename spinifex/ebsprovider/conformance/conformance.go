// Package conformance exercises the ebsprovider.EBSProvider contract
// itself, not any one implementation's internals, so it can run unmodified
// against every implementation of the interface; no assertion here has changed from the original suite it was moved out of.
package conformance

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ReferenceCapabilities is what a full-featured provider advertises. It is
// only a convenience for callers constructing a MemoryProvider; the suite
// itself never assumes any implementation reports this set.
var ReferenceCapabilities = ebsprovider.Capabilities{
	OnlineExpansion:         false,
	SparseExtentReporting:   true,
	CrashConsistentSnapshot: true,
	VolumeSeeding:           true,
	ReadOnlyPublish:         true,
	VolumeEnumeration:       true,
	SnapshotEnumeration:     true,
	// MemoryProvider keeps publication in one process's map, so node is the
	// honest answer. A reference set that claimed cluster would make the
	// suite's concurrency subtests pass against something that cannot hold.
	Exclusion: ebsprovider.ExclusionSemantics{Scope: ebsprovider.ExclusionScopeNode},
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

// maxListPages bounds a drain so a provider whose token fails to advance
// fails the test instead of hanging it.
const maxListPages = 100

// volumeIDs projects one page of refs down to its IDs.
func volumeIDs(refs []ebsprovider.VolumeRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids
}

// snapshotIDs projects one page of refs down to its IDs.
func snapshotIDs(refs []ebsprovider.SnapshotRef) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids
}

// drainVolumeIDs walks every page of ListVolumes and returns the IDs in the
// order they arrived, duplicates included: detecting a token that replays or
// overshoots a boundary is the point, so this must not deduplicate.
func drainVolumeIDs(t *testing.T, provider ebsprovider.EBSProvider, maxResults int32) []string {
	t.Helper()
	var ids []string
	var token string
	for pages := 0; ; pages++ {
		require.Lessf(t, pages, maxListPages, "ListVolumes did not terminate within %d pages; the next token is not advancing", maxListPages)
		resp, err := provider.ListVolumes(context.Background(), ebsprovider.ListVolumesRequest{
			Versioned:     ebsprovider.NewVersioned(),
			MaxResults:    maxResults,
			StartingToken: token,
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		for _, ref := range resp.Volumes {
			ids = append(ids, ref.ID)
		}
		if resp.NextToken == "" {
			return ids
		}
		token = resp.NextToken
	}
}

// drainSnapshotIDs is drainVolumeIDs for snapshots, and keeps duplicates for
// the same reason: a token that replays a boundary must be visible.
func drainSnapshotIDs(t *testing.T, provider ebsprovider.EBSProvider, maxResults int32) []string {
	t.Helper()
	var ids []string
	var token string
	for pages := 0; ; pages++ {
		require.Lessf(t, pages, maxListPages, "ListSnapshots did not terminate within %d pages; the next token is not advancing", maxListPages)
		resp, err := provider.ListSnapshots(context.Background(), ebsprovider.ListSnapshotsRequest{
			Versioned:     ebsprovider.NewVersioned(),
			MaxResults:    maxResults,
			StartingToken: token,
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		for _, ref := range resp.Snapshots {
			ids = append(ids, ref.ID)
		}
		if resp.NextToken == "" {
			return ids
		}
		token = resp.NextToken
	}
}

// SuiteConfig tunes a run for the provider it is pointed at.
type SuiteConfig struct {
	// NamePrefix is inserted into every volume and snapshot ID the suite
	// creates, after the vol-/snap- token. A provider that keeps state across
	// runs needs it: without it a second run collides with the first run's
	// volumes and fails on already_exists.
	NamePrefix string

	// NodeID is the node PublishVolume targets. A live provider answers only
	// for the node it runs on, so the in-process default is wrong there.
	NodeID string

	// OtherNodeID is a second node, used to prove a published volume cannot
	// also be published elsewhere. Empty skips that subtest, for a caller with
	// only one node to offer.
	OtherNodeID string
}

func (c SuiteConfig) node() string {
	if c.NodeID == "" {
		return "node-1"
	}
	return c.NodeID
}

func (c SuiteConfig) otherNode() string {
	if c.NodeID == "" && c.OtherNodeID == "" {
		return "node-2"
	}
	return c.OtherNodeID
}

// id builds a suite-owned identifier, keeping the vol-/snap- prefix the rest of
// the system reads as a type token.
func (c SuiteConfig) id(base string) string {
	if c.NamePrefix == "" {
		return base
	}
	if kind, rest, ok := strings.Cut(base, "-"); ok {
		return kind + "-" + c.NamePrefix + rest
	}
	return c.NamePrefix + base
}

// RunSuite runs the full EBSProvider conformance suite against a provider
// newProvider constructs fresh for each subtest.
func RunSuite(t *testing.T, newProvider func(t *testing.T) ebsprovider.EBSProvider) {
	RunSuiteWithConfig(t, newProvider, SuiteConfig{})
}

// RunSuiteWithConfig is RunSuite with control over the identifiers it uses, so
// the same suite can run against a live provider whose state outlives the test.
// Optional behaviour is gated on what the provider advertises, never on a set
// the suite picked.
func RunSuiteWithConfig(t *testing.T, newRawProvider func(t *testing.T) ebsprovider.EBSProvider, cfg SuiteConfig) {
	// Everything the suite creates is tracked and torn down, so a live
	// provider is left as the run found it.
	newProvider := func(t *testing.T) ebsprovider.EBSProvider {
		return newTrackingProvider(t, newRawProvider(t))
	}

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
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-create-ok"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
			})
			require.NoError(t, err)
			require.NotNil(t, vol)
			assert.Equal(t, cfg.id("vol-create-ok"), vol.ID)
			assert.Equal(t, int64(1<<30), vol.CapacityBytes)
			assert.Equal(t, ebsprovider.VolumeStateAvailable, vol.State)
			assert.NotEmpty(t, vol.Handle)
		})

		t.Run("already_exists on conflicting recreate", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-conflict"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-conflict"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
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
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-from-missing-snap"),
				CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
				SourceSnapshotID: cfg.id("snap-missing"), SourceSnapshotVolumeID: cfg.id("vol-missing-origin"),
			})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		// SourceSnapshotVolumeID names the volume the snapshot came from, which
		// a provider may need to resolve the snapshot's blocks. Omitting it is a
		// malformed request, not a request for behaviour the provider lacks.
		t.Run("invalid_argument when a source snapshot has no source volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-snapsrc-noorigin"),
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}, SourceSnapshotID: cfg.id("snap-any"),
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("volume is created from a snapshot", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-snapsrc-origin"),
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
			})
			require.NoError(t, err)
			snap, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-snapsrc"), VolumeID: cfg.id("vol-snapsrc-origin"),
			})
			require.NoError(t, err)
			require.Equal(t, cfg.id("vol-snapsrc-origin"), snap.SourceVolumeID, "a snapshot must report the volume it was taken from")

			restored, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-snapsrc-restored"),
				CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
				SourceSnapshotID: snap.ID, SourceSnapshotVolumeID: snap.SourceVolumeID,
			})
			require.NoError(t, err)
			assert.Equal(t, ebsprovider.VolumeStateAvailable, restored.State)
		})

		// The snapshot records its own origin, so a request naming a different
		// one is checkable and wrong. Accepting it resolves the snapshot's
		// blocks against a volume they were never written to.
		t.Run("invalid_argument on a source volume the snapshot did not come from", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-wrongorigin-src"),
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
			})
			require.NoError(t, err)
			snap, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-wrongorigin"), VolumeID: cfg.id("vol-wrongorigin-src"),
			})
			require.NoError(t, err)

			_, err = provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-wrongorigin-restored"),
				CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
				SourceSnapshotID: snap.ID, SourceSnapshotVolumeID: cfg.id("vol-wrongorigin-other"),
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("unsupported_version", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				VolumeID: cfg.id("vol-noversion"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
			})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)
		})

		t.Run("seeded volume is created", func(t *testing.T) {
			if !capabilities.VolumeSeeding {
				t.Skip("provider does not advertise VolumeSeeding")
			}
			provider := newProvider(t)
			vol, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-seeded"),
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
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-seed-unsupported"),
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
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-seed-toobig"),
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
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-seed-overcap"),
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
			created, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-get-ok"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			got, err := provider.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-get-ok")})
			require.NoError(t, err)
			assert.Equal(t, created.Handle, got.Handle)
			assert.Equal(t, created.CapacityBytes, got.CapacityBytes)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-never-existed")})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("invalid_argument on empty volume id", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})
	})

	t.Run("ListVolumes", func(t *testing.T) {
		capabilities := capabilitiesOf(t, newProvider(t))

		t.Run("unsupported_capability when enumeration is not advertised", func(t *testing.T) {
			if capabilities.VolumeEnumeration {
				t.Skip("provider advertises volume enumeration")
			}
			_, err := newProvider(t).ListVolumes(context.Background(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedCapability)
		})

		if !capabilities.VolumeEnumeration {
			return
		}

		t.Run("a created volume is enumerated and a deleted one is not", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			volumeID := cfg.id("vol-list-visible")
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			assert.Contains(t, drainVolumeIDs(t, provider, 0), volumeID,
				"enumeration must report a volume the provider holds; it is the only way to find one whose control-plane document is lost")

			require.NoError(t, provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID}))
			assert.NotContains(t, drainVolumeIDs(t, provider, 0), volumeID,
				"a deleted volume must stop being enumerated, or the report it feeds accuses storage of holding blocks it released")
		})

		t.Run("pagination returns every volume exactly once", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			want := []string{cfg.id("vol-list-page-a"), cfg.id("vol-list-page-b"), cfg.id("vol-list-page-c")}
			for _, volumeID := range want {
				_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
				require.NoError(t, err)
			}

			// One volume per page forces every token boundary to be exercised.
			paged := drainVolumeIDs(t, provider, 1)
			seen := make(map[string]int, len(paged))
			for _, id := range paged {
				seen[id]++
			}
			for id, count := range seen {
				assert.Equalf(t, 1, count, "volume %s appeared %d times across pages; a duplicate means the token replays a boundary", id, count)
			}
			for _, volumeID := range want {
				assert.Containsf(t, paged, volumeID, "volume %s was skipped across pages; a gap means the token overshoots", volumeID)
			}
		})

		t.Run("max results above the cap is clamped, not refused", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			want := []string{cfg.id("vol-list-cap-a"), cfg.id("vol-list-cap-b")}
			for _, volumeID := range want {
				_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
				require.NoError(t, err)
			}

			resp, err := provider.ListVolumes(ctx, ebsprovider.ListVolumesRequest{
				Versioned:  ebsprovider.NewVersioned(),
				MaxResults: ebsprovider.MaxListResults * 10,
			})
			require.NoError(t, err)
			require.NotNil(t, resp)
			for _, volumeID := range want {
				assert.Containsf(t, volumeIDs(resp.Volumes), volumeID,
					"volume %s went missing from an over-cap request; asking for more than fits must still return a page, not an empty one", volumeID)
			}
		})

		t.Run("unsupported_version", func(t *testing.T) {
			_, err := newProvider(t).ListVolumes(context.Background(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.Versioned{SchemaVersion: ebsprovider.SchemaVersion + 1}})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)
		})
	})

	t.Run("ExpandVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-expand-ok"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			expanded, err := provider.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-expand-ok"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
			require.NoError(t, err)
			assert.Equal(t, int64(2<<30), expanded.CapacityBytes)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.ExpandVolume(context.Background(), ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-never-existed"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("invalid_argument on shrink", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-shrink"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
			require.NoError(t, err)
			_, err = provider.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-shrink"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		// Expanding a published volume is the one operation OnlineExpansion
		// describes, so both answers are contractual: succeed, or refuse with
		// volume_in_use. Silently doing nothing is neither.
		t.Run("expanding a published volume matches OnlineExpansion", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-expand-inuse"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-expand-inuse"), NodeID: cfg.node()})
			require.NoError(t, err)

			expanded, err := provider.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-expand-inuse"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 2 << 30}})
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
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-delete-ok"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			require.NoError(t, provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-delete-ok")}))
			_, err = provider.GetVolume(ctx, ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-delete-ok")})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		// CSI's controller service treats DeleteVolume on an absent volume as
		// success (idempotent-when-absent), not not_found.
		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-never-existed")}))
		})

		t.Run("volume_in_use when published", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-delete-inuse"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-delete-inuse"), NodeID: cfg.node()})
			require.NoError(t, err)
			err = provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-delete-inuse")})
			require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
		})
	})

	t.Run("CreateSnapshot", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-snap-src"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			snap, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-ok"), VolumeID: cfg.id("vol-snap-src")})
			require.NoError(t, err)
			require.NotNil(t, snap)
			assert.Equal(t, cfg.id("snap-ok"), snap.ID)
			assert.Equal(t, cfg.id("vol-snap-src"), snap.SourceVolumeID)
			assert.Equal(t, ebsprovider.SnapshotStateCompleted, snap.State)
		})

		t.Run("not_found on absent source volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-orphan"), VolumeID: cfg.id("vol-never-existed")})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("already_exists on conflicting source volume", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-snap-a"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-snap-b"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-conflict"), VolumeID: cfg.id("vol-snap-a")})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-conflict"), VolumeID: cfg.id("vol-snap-b")})
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
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-delsnap-src"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-delete-ok"), VolumeID: cfg.id("vol-delsnap-src")})
			require.NoError(t, err)
			require.NoError(t, provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-delete-ok")}))
		})

		// memory.go implements delete-of-absent-snapshot as a no-op success,
		// matching CSI's idempotency rule the same way DeleteVolume does.
		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-never-existed")}))
		})
	})

	t.Run("CopySnapshot", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-copysnap-src"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-copysnap-src"), VolumeID: cfg.id("vol-copysnap-src")})
			require.NoError(t, err)

			copied, err := provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: cfg.id("snap-copysnap-src"), DestinationSnapshotID: cfg.id("snap-copysnap-dst"), VolumeID: cfg.id("vol-copysnap-src"),
			})
			require.NoError(t, err)
			require.NotNil(t, copied)
			assert.Equal(t, cfg.id("snap-copysnap-dst"), copied.ID)
			assert.Equal(t, cfg.id("vol-copysnap-src"), copied.SourceVolumeID)
			assert.Equal(t, ebsprovider.SnapshotStateCompleted, copied.State)
			assert.NotEmpty(t, copied.Handle)

			// The destination is a real, independently addressable snapshot.
			require.NoError(t, provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-copysnap-src")}))
			require.NoError(t, provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-copysnap-dst")}))
		})

		t.Run("not_found on absent source snapshot", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-copysnap-nosrc"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: cfg.id("snap-copysnap-missing"), DestinationSnapshotID: cfg.id("snap-copysnap-missing-dst"), VolumeID: cfg.id("vol-copysnap-nosrc"),
			})
			require.ErrorIs(t, err, ebsprovider.ErrNotFound)
		})

		t.Run("already_exists on conflicting destination", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-copysnap-conflict"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-copysnap-a"), VolumeID: cfg.id("vol-copysnap-conflict")})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-copysnap-b"), VolumeID: cfg.id("vol-copysnap-conflict")})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: cfg.id("snap-copysnap-a"), DestinationSnapshotID: cfg.id("snap-copysnap-b"), VolumeID: cfg.id("vol-copysnap-conflict"),
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
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-copysnap-same"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-copysnap-same"), VolumeID: cfg.id("vol-copysnap-same")})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: cfg.id("snap-copysnap-same"), DestinationSnapshotID: cfg.id("snap-copysnap-same"), VolumeID: cfg.id("vol-copysnap-same"),
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("invalid_argument when volume id does not own the source snapshot", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-copysnap-owner"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-copysnap-foreign"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: cfg.id("snap-copysnap-owned"), VolumeID: cfg.id("vol-copysnap-owner")})
			require.NoError(t, err)
			_, err = provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
				Versioned: ebsprovider.NewVersioned(), SourceSnapshotID: cfg.id("snap-copysnap-owned"), DestinationSnapshotID: cfg.id("snap-copysnap-owned-dst"), VolumeID: cfg.id("vol-copysnap-foreign"),
			})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})

		t.Run("unsupported_version", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
				SourceSnapshotID: cfg.id("snap-a"), DestinationSnapshotID: cfg.id("snap-b"), VolumeID: cfg.id("vol-a"),
			})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)
		})
	})

	t.Run("ListSnapshots", func(t *testing.T) {
		capabilities := capabilitiesOf(t, newProvider(t))

		t.Run("unsupported_capability when enumeration is not advertised", func(t *testing.T) {
			if capabilities.SnapshotEnumeration {
				t.Skip("provider advertises snapshot enumeration")
			}
			_, err := newProvider(t).ListSnapshots(context.Background(), ebsprovider.ListSnapshotsRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedCapability)
		})

		if !capabilities.SnapshotEnumeration {
			return
		}

		t.Run("a created snapshot is enumerated and a deleted one is not", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			volumeID := cfg.id("vol-snaplist-src")
			snapshotID := cfg.id("snap-list-visible")
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volumeID})
			require.NoError(t, err)
			assert.Contains(t, drainSnapshotIDs(t, provider, 0), snapshotID,
				"enumeration must report a snapshot the provider holds; it is the only way to find one whose control-plane document is lost")

			require.NoError(t, provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID}))
			assert.NotContains(t, drainSnapshotIDs(t, provider, 0), snapshotID,
				"a deleted snapshot must stop being enumerated, or the report it feeds accuses storage of holding data it released")
		})

		t.Run("pagination returns every snapshot exactly once", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			volumeID := cfg.id("vol-snaplist-page-src")
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			want := []string{cfg.id("snap-list-page-a"), cfg.id("snap-list-page-b"), cfg.id("snap-list-page-c")}
			for _, snapshotID := range want {
				_, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volumeID})
				require.NoError(t, err)
			}

			// One snapshot per page forces every token boundary to be exercised.
			paged := drainSnapshotIDs(t, provider, 1)
			seen := make(map[string]int, len(paged))
			for _, id := range paged {
				seen[id]++
			}
			for id, count := range seen {
				assert.Equalf(t, 1, count, "snapshot %s appeared %d times across pages; a duplicate means the token replays a boundary", id, count)
			}
			for _, snapshotID := range want {
				assert.Containsf(t, paged, snapshotID, "snapshot %s was skipped across pages; a gap means the token overshoots", snapshotID)
			}
		})

		t.Run("max results above the cap is clamped, not refused", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			volumeID := cfg.id("vol-snaplist-cap-src")
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			want := []string{cfg.id("snap-list-cap-a"), cfg.id("snap-list-cap-b")}
			for _, snapshotID := range want {
				_, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volumeID})
				require.NoError(t, err)
			}

			resp, err := provider.ListSnapshots(ctx, ebsprovider.ListSnapshotsRequest{
				Versioned:  ebsprovider.NewVersioned(),
				MaxResults: ebsprovider.MaxListResults * 10,
			})
			require.NoError(t, err)
			require.NotNil(t, resp)
			for _, snapshotID := range want {
				assert.Containsf(t, snapshotIDs(resp.Snapshots), snapshotID,
					"snapshot %s went missing from an over-cap request; asking for more than fits must still return a page, not an empty one", snapshotID)
			}
		})

		t.Run("unsupported_version", func(t *testing.T) {
			_, err := newProvider(t).ListSnapshots(context.Background(), ebsprovider.ListSnapshotsRequest{Versioned: ebsprovider.Versioned{SchemaVersion: ebsprovider.SchemaVersion + 1}})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)
		})
	})

	t.Run("PublishVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-ok"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			pub, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-ok"), NodeID: cfg.node()})
			require.NoError(t, err)
			require.NotNil(t, pub)
			assert.Equal(t, cfg.id("vol-pub-ok"), pub.VolumeID)
			assert.Equal(t, cfg.node(), pub.NodeID)
			assertNBDURI(t, pub.NBDURI)
		})

		t.Run("idempotent republish to the same node", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-idem"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			first, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-idem"), NodeID: cfg.node()})
			require.NoError(t, err)
			second, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-idem"), NodeID: cfg.node()})
			require.NoError(t, err)
			assert.Equal(t, first, second)
		})

		t.Run("volume_in_use when published to a different node", func(t *testing.T) {
			if cfg.otherNode() == "" {
				t.Skip("no second node configured")
			}
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-conflict"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-conflict"), NodeID: cfg.node()})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-pub-conflict"), NodeID: cfg.otherNode()})
			require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-never-existed"), NodeID: cfg.node()})
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
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-unpub-ok"), CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-unpub-ok"), NodeID: cfg.node()})
			require.NoError(t, err)
			require.NoError(t, provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-unpub-ok"), NodeID: cfg.node()}))
		})

		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.id("vol-never-existed"), NodeID: cfg.node()}))
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			err := provider.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned()})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
		})
	})

	t.Run("Exclusion", func(t *testing.T) {
		exclusion := capabilities.Exclusion

		// An advertised guarantee that contradicts itself is worse than a
		// missing one: a caller reads it and plans around something the
		// provider never claimed to do.
		t.Run("advertised semantics are self-consistent", func(t *testing.T) {
			switch exclusion.Scope {
			case ebsprovider.ExclusionScopeNone, ebsprovider.ExclusionScopeNode, ebsprovider.ExclusionScopeCluster:
			default:
				t.Fatalf("unknown exclusion scope %q; a caller cannot branch on a scope it does not recognise", exclusion.Scope)
			}
			if exclusion.Scope == ebsprovider.ExclusionScopeNone {
				assert.Zero(t, exclusion.ClaimTTLSeconds, "a provider that excludes nothing holds no claim, so a TTL on it means nothing")
				assert.False(t, exclusion.FencesLostClaim, "there is no claim to lose, so nothing can be fenced on losing it")
			}
			assert.GreaterOrEqual(t, exclusion.ClaimTTLSeconds, 0, "a negative TTL is not a duration")
			if exclusion.FencesLostClaim {
				assert.NotEqual(t, ebsprovider.ExclusionScopeNone, exclusion.Scope,
					"fencing a lost claim requires there to be a claim")
			}
			assert.Equal(t, exclusion.Scope == ebsprovider.ExclusionScopeCluster, exclusion.SingleWriter(),
				"SingleWriter must agree with Scope, or callers branching on it get a different answer from callers reading the scope")
		})

		if exclusion.Scope == ebsprovider.ExclusionScopeNone {
			return
		}

		t.Run("a second publish elsewhere is refused while the first holds", func(t *testing.T) {
			if cfg.otherNode() == "" {
				t.Skip("no second node configured")
			}
			provider := newProvider(t)
			ctx := context.Background()
			volumeID := cfg.id("vol-excl-second")
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: cfg.node()})
			require.NoError(t, err)

			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: cfg.otherNode()})
			require.ErrorIsf(t, err, ebsprovider.ErrVolumeInUse,
				"scope %q promises a second writer is refused; letting this through is the two-writer case the guarantee exists to prevent", exclusion.Scope)

			// Releasing the claim must make the volume publishable again, or
			// the guarantee is indistinguishable from a permanent lock.
			require.NoError(t, provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: cfg.node()}))
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: cfg.otherNode()})
			require.NoError(t, err, "a released claim must be reclaimable, or an unpublish strands the volume")
		})

		// Concurrent publishes settle to one winner. Both succeeding is the
		// corruption case; both failing means the claim was lost by neither
		// and the volume is stranded.
		t.Run("racing publishes from two nodes produce exactly one winner", func(t *testing.T) {
			if cfg.otherNode() == "" {
				t.Skip("no second node configured")
			}
			provider := newProvider(t)
			ctx := context.Background()
			volumeID := cfg.id("vol-excl-race")
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)

			nodes := []string{cfg.node(), cfg.otherNode()}
			errs := make([]error, len(nodes))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i, node := range nodes {
				wg.Go(func() {
					<-start
					_, errs[i] = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{
						Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: node,
					})
				})
			}
			close(start)
			wg.Wait()

			var winners int
			for i, err := range errs {
				if err == nil {
					winners++
					continue
				}
				assert.ErrorIsf(t, err, ebsprovider.ErrVolumeInUse,
					"the loser of a publish race must be told the volume is in use, not %v; node %s cannot retry sensibly otherwise", err, nodes[i])
			}
			assert.Equalf(t, 1, winners, "%d of %d racing publishes succeeded; two winners is the two-writer case, zero strands the volume", winners, len(nodes))
		})

		// A delete arriving while a volume is published must lose to the
		// publication, not race it. Deleting under a live writer destroys
		// blocks the writer still believes it owns.
		t.Run("delete racing a live publication is refused", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			volumeID := cfg.id("vol-excl-delrace")
			_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: cfg.node()})
			require.NoError(t, err)

			err = provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID})
			require.ErrorIs(t, err, ebsprovider.ErrVolumeInUse,
				"deleting a published volume must be refused; the export is still serving reads and writes against it")

			require.NoError(t, provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: cfg.node()}))
			require.NoError(t, provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID}),
				"once unpublished the delete must go through, or a volume can never be removed after being used")
		})
	})
}
