package ebsprovider

// runConformanceSuite exercises the EBSProvider contract itself, not any one
// implementation's internals. It runs unmodified against MemoryProvider and
// against a NATSProvider wired to a small test-only server that delegates to
// a fresh MemoryProvider, so both sides are checked against the interface
// contract rather than against each other's structs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conformanceCapabilities is shared by both provider constructors under test
// so GetCapabilities and the OnlineExpansion-gated volume_in_use case behave
// identically regardless of which EBSProvider implementation answers.
var conformanceCapabilities = Capabilities{
	CopyOnWriteClone:        true,
	OnlineExpansion:         false,
	SparseExtentReporting:   false,
	CrashConsistentSnapshot: true,
	VolumeSeeding:           true,
}

func TestMemoryProviderConformance(t *testing.T) {
	runConformanceSuite(t, func(t *testing.T) EBSProvider {
		t.Helper()
		return NewMemoryProvider(conformanceCapabilities)
	})
}

func TestNATSProviderConformance(t *testing.T) {
	runConformanceSuite(t, func(t *testing.T) EBSProvider {
		t.Helper()
		_, conn := testutil.StartTestNATS(t)
		startConformanceServer(t, conn, NewMemoryProvider(conformanceCapabilities))
		return NewNATSProvider(conn, 5*time.Second)
	})
}

func runConformanceSuite(t *testing.T, newProvider func(t *testing.T) EBSProvider) {
	t.Run("GetCapabilities", func(t *testing.T) {
		provider := newProvider(t)
		resp, err := provider.GetCapabilities(context.Background(), GetCapabilitiesRequest{Versioned: NewVersioned()})
		require.NoError(t, err)
		assert.Equal(t, conformanceCapabilities, resp.Capabilities)
	})

	t.Run("CreateVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			vol, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
				Versioned: NewVersioned(), VolumeID: "vol-create-ok", CapacityRange: CapacityRange{RequiredBytes: 1 << 30},
			})
			require.NoError(t, err)
			require.NotNil(t, vol)
			assert.Equal(t, "vol-create-ok", vol.ID)
			assert.Equal(t, int64(1<<30), vol.CapacityBytes)
			assert.Equal(t, VolumeStateAvailable, vol.State)
			assert.NotEmpty(t, vol.Handle)
		})

		t.Run("already_exists on conflicting recreate", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-conflict", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-conflict", CapacityRange: CapacityRange{RequiredBytes: 2 << 30}})
			require.ErrorIs(t, err, ErrAlreadyExists)
		})

		t.Run("invalid_argument on empty volume id", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{Versioned: NewVersioned(), CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})

		t.Run("not_found on absent source snapshot", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
				Versioned: NewVersioned(), VolumeID: "vol-from-missing-snap",
				CapacityRange: CapacityRange{RequiredBytes: 1 << 30}, SourceSnapshotID: "snap-missing",
			})
			require.ErrorIs(t, err, ErrNotFound)
		})

		t.Run("unsupported_version", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
				VolumeID: "vol-noversion", CapacityRange: CapacityRange{RequiredBytes: 1 << 30},
			})
			require.ErrorIs(t, err, ErrUnsupportedVersion)
		})

		t.Run("seeded volume is created", func(t *testing.T) {
			provider := newProvider(t)
			vol, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
				Versioned: NewVersioned(), VolumeID: "vol-seeded",
				CapacityRange: CapacityRange{RequiredBytes: 4096},
				SeedData:      bytes.Repeat([]byte{0xAB}, 4096),
			})
			require.NoError(t, err)
			require.NotNil(t, vol)
			assert.Equal(t, int64(4096), vol.CapacityBytes)
		})

		// A seed above MaxSeedBytes must fail as invalid_argument rather than
		// reaching the transport, where NATS would refuse the oversized publish
		// with an error that says nothing about firmware.
		t.Run("invalid_argument on oversized seed", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
				Versioned: NewVersioned(), VolumeID: "vol-seed-toobig",
				CapacityRange: CapacityRange{RequiredBytes: 1 << 30},
				SeedData:      make([]byte, MaxSeedBytes+1),
			})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})

		t.Run("invalid_argument when seed exceeds capacity", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
				Versioned: NewVersioned(), VolumeID: "vol-seed-overcap",
				CapacityRange: CapacityRange{RequiredBytes: 512},
				SeedData:      make([]byte, 4096),
			})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})
	})

	t.Run("GetVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			created, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-get-ok", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			got, err := provider.GetVolume(ctx, GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-get-ok"})
			require.NoError(t, err)
			assert.Equal(t, created.Handle, got.Handle)
			assert.Equal(t, created.CapacityBytes, got.CapacityBytes)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.GetVolume(context.Background(), GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-never-existed"})
			require.ErrorIs(t, err, ErrNotFound)
		})

		t.Run("invalid_argument on empty volume id", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.GetVolume(context.Background(), GetVolumeRequest{Versioned: NewVersioned()})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})
	})

	t.Run("ExpandVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-expand-ok", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			expanded, err := provider.ExpandVolume(ctx, ExpandVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-expand-ok", CapacityRange: CapacityRange{RequiredBytes: 2 << 30}})
			require.NoError(t, err)
			assert.Equal(t, int64(2<<30), expanded.CapacityBytes)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.ExpandVolume(context.Background(), ExpandVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-never-existed", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ErrNotFound)
		})

		t.Run("invalid_argument on shrink", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-shrink", CapacityRange: CapacityRange{RequiredBytes: 2 << 30}})
			require.NoError(t, err)
			_, err = provider.ExpandVolume(ctx, ExpandVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-shrink", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})

		t.Run("volume_in_use when published and provider lacks online expansion", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-expand-inuse", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-expand-inuse", NodeID: "node-1"})
			require.NoError(t, err)
			_, err = provider.ExpandVolume(ctx, ExpandVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-expand-inuse", CapacityRange: CapacityRange{RequiredBytes: 2 << 30}})
			require.ErrorIs(t, err, ErrVolumeInUse)
		})
	})

	t.Run("DeleteVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-delete-ok", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			require.NoError(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-delete-ok"}))
			_, err = provider.GetVolume(ctx, GetVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-delete-ok"})
			require.ErrorIs(t, err, ErrNotFound)
		})

		// CSI's controller service treats DeleteVolume on an absent volume as
		// success (idempotent-when-absent), not not_found.
		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.DeleteVolume(context.Background(), DeleteVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-never-existed"}))
		})

		t.Run("volume_in_use when published", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-delete-inuse", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-delete-inuse", NodeID: "node-1"})
			require.NoError(t, err)
			err = provider.DeleteVolume(ctx, DeleteVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-delete-inuse"})
			require.ErrorIs(t, err, ErrVolumeInUse)
		})
	})

	t.Run("CreateSnapshot", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-snap-src", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			snap, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-ok", VolumeID: "vol-snap-src"})
			require.NoError(t, err)
			require.NotNil(t, snap)
			assert.Equal(t, "snap-ok", snap.ID)
			assert.Equal(t, "vol-snap-src", snap.SourceVolumeID)
			assert.Equal(t, SnapshotStateCompleted, snap.State)
		})

		t.Run("not_found on absent source volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateSnapshot(context.Background(), CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-orphan", VolumeID: "vol-never-existed"})
			require.ErrorIs(t, err, ErrNotFound)
		})

		t.Run("already_exists on conflicting source volume", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-snap-a", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-snap-b", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-conflict", VolumeID: "vol-snap-a"})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-conflict", VolumeID: "vol-snap-b"})
			require.ErrorIs(t, err, ErrAlreadyExists)
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.CreateSnapshot(context.Background(), CreateSnapshotRequest{Versioned: NewVersioned()})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})
	})

	t.Run("DeleteSnapshot", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-delsnap-src", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.CreateSnapshot(ctx, CreateSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-delete-ok", VolumeID: "vol-delsnap-src"})
			require.NoError(t, err)
			require.NoError(t, provider.DeleteSnapshot(ctx, DeleteSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-delete-ok"}))
		})

		// memory.go implements delete-of-absent-snapshot as a no-op success,
		// matching CSI's idempotency rule the same way DeleteVolume does.
		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.DeleteSnapshot(context.Background(), DeleteSnapshotRequest{Versioned: NewVersioned(), SnapshotID: "snap-never-existed"}))
		})
	})

	t.Run("PublishVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-ok", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			pub, err := provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-ok", NodeID: "node-1"})
			require.NoError(t, err)
			require.NotNil(t, pub)
			assert.Equal(t, "vol-pub-ok", pub.VolumeID)
			assert.Equal(t, "node-1", pub.NodeID)
			assert.NotEmpty(t, pub.NBDURI)
		})

		t.Run("idempotent republish to the same node", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-idem", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			first, err := provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-idem", NodeID: "node-1"})
			require.NoError(t, err)
			second, err := provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-idem", NodeID: "node-1"})
			require.NoError(t, err)
			assert.Equal(t, first, second)
		})

		t.Run("volume_in_use when published to a different node", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-conflict", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-conflict", NodeID: "node-1"})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-pub-conflict", NodeID: "node-2"})
			require.ErrorIs(t, err, ErrVolumeInUse)
		})

		t.Run("not_found on absent volume", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.PublishVolume(context.Background(), PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-never-existed", NodeID: "node-1"})
			require.ErrorIs(t, err, ErrNotFound)
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			_, err := provider.PublishVolume(context.Background(), PublishVolumeRequest{Versioned: NewVersioned()})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})
	})

	t.Run("UnpublishVolume", func(t *testing.T) {
		t.Run("success", func(t *testing.T) {
			provider := newProvider(t)
			ctx := context.Background()
			_, err := provider.CreateVolume(ctx, CreateVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-unpub-ok", CapacityRange: CapacityRange{RequiredBytes: 1 << 30}})
			require.NoError(t, err)
			_, err = provider.PublishVolume(ctx, PublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-unpub-ok", NodeID: "node-1"})
			require.NoError(t, err)
			require.NoError(t, provider.UnpublishVolume(ctx, UnpublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-unpub-ok", NodeID: "node-1"}))
		})

		t.Run("absent target is idempotent", func(t *testing.T) {
			provider := newProvider(t)
			require.NoError(t, provider.UnpublishVolume(context.Background(), UnpublishVolumeRequest{Versioned: NewVersioned(), VolumeID: "vol-never-existed", NodeID: "node-1"}))
		})

		t.Run("invalid_argument on missing ids", func(t *testing.T) {
			provider := newProvider(t)
			err := provider.UnpublishVolume(context.Background(), UnpublishVolumeRequest{Versioned: NewVersioned()})
			require.ErrorIs(t, err, ErrInvalidArgument)
		})
	})
}

// startConformanceServer subscribes every ebs.provider.v1.* subject on conn
// and delegates each request to backing, turning a plain MemoryProvider into
// the wire-compatible twin the conformance suite drives through NATSProvider.
func startConformanceServer(t *testing.T, conn *nats.Conn, backing EBSProvider) {
	t.Helper()
	ctx := context.Background()

	subscribe := func(subject string, handler nats.MsgHandler) {
		sub, err := conn.Subscribe(subject, handler)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	subscribe(CapabilitiesSubject, func(msg *nats.Msg) {
		var req GetCapabilitiesRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, GetCapabilitiesResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		resp, err := backing.GetCapabilities(ctx, req)
		if err != nil {
			conformanceRespond(t, msg, GetCapabilitiesResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, *resp)
	})

	subscribe(CreateVolumeSubject, func(msg *nats.Msg) {
		var req CreateVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, CreateVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		vol, err := backing.CreateVolume(ctx, req)
		if err != nil {
			conformanceRespond(t, msg, CreateVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, CreateVolumeResponse{Versioned: NewVersioned(), Volume: vol})
	})

	subscribe(GetVolumeSubject, func(msg *nats.Msg) {
		var req GetVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, GetVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		vol, err := backing.GetVolume(ctx, req)
		if err != nil {
			conformanceRespond(t, msg, GetVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, GetVolumeResponse{Versioned: NewVersioned(), Volume: vol})
	})

	subscribe(ExpandVolumeSubject, func(msg *nats.Msg) {
		var req ExpandVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, ExpandVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		vol, err := backing.ExpandVolume(ctx, req)
		if err != nil {
			conformanceRespond(t, msg, ExpandVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, ExpandVolumeResponse{Versioned: NewVersioned(), Volume: vol})
	})

	subscribe(DeleteVolumeSubject, func(msg *nats.Msg) {
		var req DeleteVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, DeleteVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		if err := backing.DeleteVolume(ctx, req); err != nil {
			conformanceRespond(t, msg, DeleteVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, DeleteVolumeResponse{Versioned: NewVersioned()})
	})

	subscribe(DeleteSnapshotSubject, func(msg *nats.Msg) {
		var req DeleteSnapshotRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, DeleteSnapshotResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		if err := backing.DeleteSnapshot(ctx, req); err != nil {
			conformanceRespond(t, msg, DeleteSnapshotResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, DeleteSnapshotResponse{Versioned: NewVersioned()})
	})

	// CreateSnapshot's request subject carries the source volume ID
	// (SnapshotCreateSubjectPrefix + volumeID); this test server answers
	// synchronously since MemoryProvider.CreateSnapshot never returns
	// SnapshotStatePending, so NATSProvider.CreateSnapshot takes its
	// immediate-completion branch and never waits on a completion subject.
	subscribe(SnapshotCreateSubjectPrefix+"*", func(msg *nats.Msg) {
		var req CreateSnapshotRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, CreateSnapshotResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		snap, err := backing.CreateSnapshot(ctx, req)
		if err != nil {
			conformanceRespond(t, msg, CreateSnapshotResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, CreateSnapshotResponse{Versioned: NewVersioned(), Snapshot: snap})
	})

	// Publish/Unpublish subjects carry the node ID as their own token; the
	// request body already carries the same NodeID, so the handler needs no
	// subject parsing.
	subscribe("ebs.provider.v1.*.mount", func(msg *nats.Msg) {
		var req PublishVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, PublishVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		pub, err := backing.PublishVolume(ctx, req)
		if err != nil {
			conformanceRespond(t, msg, PublishVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, PublishVolumeResponse{Versioned: NewVersioned(), Published: pub})
	})

	subscribe("ebs.provider.v1.*.unmount", func(msg *nats.Msg) {
		var req UnpublishVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			conformanceRespond(t, msg, UnpublishVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		if err := backing.UnpublishVolume(ctx, req); err != nil {
			conformanceRespond(t, msg, UnpublishVolumeResponse{Versioned: NewVersioned(), Error: conformanceProviderError(err)})
			return
		}
		conformanceRespond(t, msg, UnpublishVolumeResponse{Versioned: NewVersioned()})
	})

	require.NoError(t, conn.Flush())
}

func conformanceRespond(t *testing.T, msg *nats.Msg, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, msg.Respond(data))
}

// conformanceProviderError maps a MemoryProvider sentinel error to the wire
// ProviderError shape, mirroring how a real provider daemon (e.g.
// viperblockd's provider_handlers.go) classifies its own errors.
func conformanceProviderError(err error) *ProviderError {
	switch {
	case errors.Is(err, ErrAlreadyExists):
		return &ProviderError{Code: ErrorCodeAlreadyExists, Message: err.Error()}
	case errors.Is(err, ErrInvalidArgument):
		return &ProviderError{Code: ErrorCodeInvalidArgument, Message: err.Error()}
	case errors.Is(err, ErrNotFound):
		return &ProviderError{Code: ErrorCodeNotFound, Message: err.Error()}
	case errors.Is(err, ErrUnsupportedVersion):
		return &ProviderError{Code: ErrorCodeUnsupportedVersion, Message: err.Error()}
	case errors.Is(err, ErrVolumeInUse):
		return &ProviderError{Code: ErrorCodeVolumeInUse, Message: err.Error()}
	default:
		return &ProviderError{Code: ErrorCodeInternal, Message: err.Error()}
	}
}
