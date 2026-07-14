package handlers_ec2_volume

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVolumeLeakReaper_ReconcilesAttachmentNeverDeletesData verifies decision
// 1(b): once the owning instance is definitively terminated (this node's
// leaked set), the reaper reconciles ONLY the attachment record — via
// UpdateVolumeState to available/detached — never the volume data. This
// unblocks an explicit operator DeleteVolume without weakening ADR-0005 §3's
// mark-and-alarm, never-delete contract (TestRLC5).
func TestVolumeLeakReaper_ReconcilesAttachmentNeverDeletesData(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-leaked-reconcile", "in-use", "i-gone0000000000")

	reaper := svc.NewVolumeLeakReaper(func() (map[string]bool, error) {
		return map[string]bool{"i-gone0000000000": true}, nil
	})

	marked, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, marked)

	cfg, err := svc.GetVolumeConfig("vol-leaked-reconcile")
	require.NoError(t, err, "the reaper must never delete the volume, only mark + reconcile it")
	assert.NotEmpty(t, cfg.VolumeMetadata.Tags[orphanTagKey], "the volume must still be marked orphaned")
	assert.Equal(t, "available", cfg.VolumeMetadata.State,
		"the reaper must reconcile the attachment record to available so DeleteVolume can proceed")
	assert.Empty(t, cfg.VolumeMetadata.AttachedInstance,
		"the reaper must clear the stale attachment left by the terminated instance")

	// The reconcile only clears control-plane attachment state — this is
	// exactly the guard DeleteVolume checks (service_impl.go DeleteVolume):
	// AttachedInstance=="" and State in {"available",""}. Confirm the reaper
	// leaves the volume passing that guard, without exercising DeleteVolume's
	// full path here (which needs a snapshotKV this unit fixture omits).
	assert.True(t, cfg.VolumeMetadata.AttachedInstance == "" && (cfg.VolumeMetadata.State == "available" || cfg.VolumeMetadata.State == ""),
		"the reconciled volume must pass DeleteVolume's in-use guard so an operator can delete it")
}

// TestVolumeLeakReaper_LiveOwnerAttachmentUntouched verifies the reconcile is
// scoped to leaked (terminated, no-live-owner) volumes only: a volume
// attached to an instance NOT in the leaked set must keep its attachment
// state exactly as-is.
func TestVolumeLeakReaper_LiveOwnerAttachmentUntouched(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-live-reconcile", "in-use", "i-running00000000")

	reaper := svc.NewVolumeLeakReaper(func() (map[string]bool, error) {
		return map[string]bool{"i-gone0000000000": true}, nil
	})

	marked, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, marked)

	cfg, err := svc.GetVolumeConfig("vol-live-reconcile")
	require.NoError(t, err)
	assert.Equal(t, "in-use", cfg.VolumeMetadata.State, "a live-owner volume's attachment must never be reconciled")
	assert.Equal(t, "i-running00000000", cfg.VolumeMetadata.AttachedInstance)
}
