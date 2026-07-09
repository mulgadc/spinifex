package handlers_ec2_volume

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
)

// orphanTagKey marks a volume the GC has surfaced as orphaned. It is a
// non-destructive tag, never a delete: volumes carry customer data.
const (
	orphanTagKey         = "spinifex:orphaned"
	orphanInstanceTagKey = "spinifex:orphaned-instance"
)

// VolumeLeakReaper is ADR-0005's data-safety GC override. The shared GC backstop
// reaps actual−desired for every other resource class; for volumes that default
// is forbidden. This reaper finds a volume left attached to an instance that is
// definitively gone and MARKS it orphaned + ALARMS — it never issues a delete.
// A wrongful reap of an OVN port reboots a VM; a wrongful reap of a volume loses
// customer data, so reclamation is always an explicit operator action.
type VolumeLeakReaper struct {
	svc *VolumeServiceImpl
	// leaked returns the set of instance IDs this node owns whose teardown
	// leaked a volume (terminated here with a failed volumes-teardown). Keying
	// on this node's definitively-gone instances keeps the shared-store scan
	// from ever false-marking a volume attached to another node's live instance.
	leaked func() (map[string]bool, error)
}

var _ vm.Reaper = (*VolumeLeakReaper)(nil)

// NewVolumeLeakReaper builds the data-safety reaper. leaked supplies the
// node-owned instance IDs whose volume teardown failed.
func (s *VolumeServiceImpl) NewVolumeLeakReaper(leaked func() (map[string]bool, error)) *VolumeLeakReaper {
	return &VolumeLeakReaper{svc: s, leaked: leaked}
}

func (r *VolumeLeakReaper) Class() string         { return "volume-leak" }
func (r *VolumeLeakReaper) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep marks every volume attached to a leaked instance as orphaned and alarms.
// It never deletes — that is the deliberate data-safety exception to the GC's
// reap-actual−desired default (ADR-0005 §3).
func (r *VolumeLeakReaper) Sweep(ctx context.Context) (int, error) {
	leaked, err := r.leaked()
	if err != nil {
		return 0, err
	}
	if len(leaked) == 0 {
		return 0, nil
	}

	ids, err := r.svc.listAllVolumeIDs(ctx)
	if err != nil {
		return 0, err
	}

	marked := 0
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return marked, ctx.Err()
		default:
		}

		cfg, err := r.svc.getVolumeConfig(ctx, id)
		if err != nil {
			continue
		}
		md := &cfg.VolumeMetadata
		if md.AttachedInstance == "" || !leaked[md.AttachedInstance] {
			continue
		}
		if md.Tags[orphanTagKey] != "" {
			continue // already surfaced; idempotent
		}

		if err := r.svc.markVolumeOrphaned(ctx, id, cfg); err != nil {
			slog.Error("volume-leak: failed to mark orphaned volume", "volumeId", id, "err", err)
			continue
		}
		// ALARM — surface the retained orphan to the operator. NEVER delete:
		// reclamation is the explicit --purge / operator path (ADR-0005 §3).
		slog.Warn("DATA-SAFETY ALARM: orphaned volume retained, not deleted",
			"volumeId", id, "attachedInstance", md.AttachedInstance, "sizeGiB", md.SizeGiB)

		// Reconcile the attachment record only — never the volume data. The
		// owning instance is definitively terminated (this node's leaked
		// set), so nothing still claims the volume; clearing the stale
		// attachment lets an operator DeleteVolume it after seeing the alarm.
		if err := r.svc.UpdateVolumeState(id, "available", "", ""); err != nil {
			slog.Error("volume-leak: failed to reconcile orphaned volume attachment", "volumeId", id, "err", err)
		}
		marked++
	}
	return marked, nil
}

// markVolumeOrphaned tags the volume as orphaned and persists it. Tag-only: the
// volume's data and state are untouched so reclamation stays an explicit choice.
func (s *VolumeServiceImpl) markVolumeOrphaned(ctx context.Context, volumeID string, cfg *viperblock.VolumeConfig) error {
	if cfg.VolumeMetadata.Tags == nil {
		cfg.VolumeMetadata.Tags = make(map[string]string)
	}
	cfg.VolumeMetadata.Tags[orphanTagKey] = time.Now().UTC().Format(time.RFC3339)
	cfg.VolumeMetadata.Tags[orphanInstanceTagKey] = cfg.VolumeMetadata.AttachedInstance
	return s.putVolumeConfig(ctx, volumeID, cfg)
}
