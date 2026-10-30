package volume

import (
	"context"
	"log/slog"

	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// VolumeReclaimReaper converges the volumes a tenant has already asked to
// delete. DeleteVolume records the obligation as State="deleting" and returns,
// so the object teardown runs here: its cost is one round trip per object and
// therefore a function of the volume, which is not work a caller can be held
// on. The active record is removed only once the teardown reports complete.
//
// Its licence to delete volume data is narrow and is not VolumeLeakReaper's.
// That reaper may only mark and alarm because it would be deciding, on its own
// judgement, that a volume is garbage. This one decides nothing: the tenant
// decided, the decision is recorded on the volume itself, and the sweep only
// finishes what the API accepted.
type VolumeReclaimReaper struct {
	svc *VolumeServiceImpl
}

var _ vm.Reaper = (*VolumeReclaimReaper)(nil)

// NewVolumeReclaimReaper builds the reclaimer for volumes recorded as deleting.
func (s *VolumeServiceImpl) NewVolumeReclaimReaper() *VolumeReclaimReaper {
	return &VolumeReclaimReaper{svc: s}
}

func (r *VolumeReclaimReaper) Class() string { return "volume-reclaim" }

// Scope is cluster-wide, which is also what serialises the teardown. A delete
// that ran on every node would put several destructive handlers on one volume's
// objects at once, which is what the synchronous path did when a caller retried.
func (r *VolumeReclaimReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

// Sweep finishes every recorded deletion it can and reports how many records it
// retired. A volume whose teardown fails keeps its record and is retried on the
// next sweep: predastore being slow, saturated or down delays reclamation and
// must not undo a delete the tenant has already been told succeeded.
func (r *VolumeReclaimReaper) Sweep(ctx context.Context) (int, error) {
	if r.svc.provider == nil {
		return 0, nil
	}

	// The sweep has no caller account, so each document supplies the account its
	// own key is under.
	volumes, err := r.svc.metadata.ListAllVolumes(ctx)
	if err != nil {
		return 0, err
	}

	reclaimed := 0
	for _, meta := range volumes {
		select {
		case <-ctx.Done():
			return reclaimed, ctx.Err()
		default:
		}

		if meta.State != volumeStateDeleting {
			continue
		}

		if err := r.svc.provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{
			Versioned: ebsprovider.NewVersioned(),
			VolumeID:  meta.VolumeID,
			Handle:    meta.ProviderHandle,
		}); err != nil {
			slog.WarnContext(ctx, "volume-reclaim: teardown incomplete, retrying next sweep",
				"volumeId", meta.VolumeID, "accountId", meta.TenantID, "err", err)
			continue
		}

		// Removing the record is the last step by design: while it exists the
		// obligation is readable, so a sweep interrupted anywhere above resumes
		// rather than leaving objects nobody is accountable for.
		if err := r.svc.metadata.DeleteVolume(ctx, meta.TenantID, meta.VolumeID); err != nil && !objectstore.IsNoSuchKeyError(err) {
			slog.ErrorContext(ctx, "volume-reclaim: failed to remove the volume record",
				"volumeId", meta.VolumeID, "accountId", meta.TenantID, "err", err)
			continue
		}

		slog.InfoContext(ctx, "volume-reclaim: deletion complete",
			"volumeId", meta.VolumeID, "accountId", meta.TenantID)
		reclaimed++
	}
	return reclaimed, nil
}
