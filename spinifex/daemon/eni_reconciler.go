package daemon

import (
	"context"
	"log/slog"
	"time"

	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// eniStaleThreshold is the minimum age of an AttachmentStatus transition before
// the reconciler will resolve it. Shorter than this and a live attach/detach
// handler may still be mid-pipeline; the pipeline budget is ~5s, so 30s leaves
// generous headroom while still healing a crashed daemon promptly.
const eniStaleThreshold = 30 * time.Second

// eniReconciler converges hot-plug ENI state (KV ↔ in-memory slot map ↔ guest
// query-pci) for this node's running instances and reaps orphan devices/slots.
// It is a node-local vm.Reaper run by the GarbageCollector backstop.
type eniReconciler struct {
	vmMgr *vm.Manager
	vpc   *handlers_ec2_vpc.VPCServiceImpl
	stale time.Duration
}

var _ vm.Reaper = (*eniReconciler)(nil)

// newENIReconciler builds the reconciler over the daemon's VM manager and VPC
// service. Returns nil when either dependency is absent so the caller can skip
// wiring it on a node without a control plane.
func (d *Daemon) newENIReconciler() *eniReconciler {
	if d.vmMgr == nil || d.vpcService == nil {
		return nil
	}
	return &eniReconciler{vmMgr: d.vmMgr, vpc: d.vpcService, stale: eniStaleThreshold}
}

func (r *eniReconciler) Class() string         { return "eni-hotplug" }
func (r *eniReconciler) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep walks this node's running instances and converges each one's ENI
// hot-plug state. Per-instance failures are logged and skipped so one bad QMP
// socket does not stall the whole sweep.
func (r *eniReconciler) Sweep(ctx context.Context) (int, error) {
	reaped := 0
	for _, instance := range r.vmMgr.Snapshot() {
		if ctx.Err() != nil {
			return reaped, ctx.Err()
		}
		if r.vmMgr.Status(instance) != vm.StateRunning || instance.AccountID == "" {
			continue
		}
		reaped += r.reconcileInstance(instance)
	}
	return reaped, nil
}

// reconcileInstance applies the resolution matrix to a single instance.
func (r *eniReconciler) reconcileInstance(instance *vm.VM) int {
	live, err := r.vmMgr.ListLiveENIDevices(instance)
	if err != nil {
		slog.Warn("eni-reconciler: query-pci failed, skipping instance",
			"instanceId", instance.ID, "err", err)
		return 0
	}

	enis, err := r.vpc.ListInstanceENIs(instance.AccountID, instance.ID)
	if err != nil {
		slog.Warn("eni-reconciler: ListInstanceENIs failed, skipping instance",
			"instanceId", instance.ID, "err", err)
		return 0
	}

	reaped := 0
	claimedSlots := make(map[int]bool)
	knownENIs := make(map[string]bool)

	for i := range enis {
		rec := &enis[i]
		knownENIs[rec.NetworkInterfaceId] = true
		slot := r.slotFor(instance, rec)
		if slot > 0 {
			claimedSlots[slot] = true
		}
		if r.reconcileENI(instance, rec, slot, live) {
			reaped++
		}
	}

	reaped += r.sweepOrphanDevices(instance, live, claimedSlots)
	reaped += r.sweepOrphanSlots(instance, knownENIs)
	return reaped
}

// slotFor resolves the PCIe slot for a record: the persisted HotPlugSlot, or
// the in-memory map when a crash interrupted the attach before HotPlugSlot was
// written.
func (r *eniReconciler) slotFor(instance *vm.VM, rec *handlers_ec2_vpc.ENIRecord) int {
	if rec.HotPlugSlot > 0 {
		return rec.HotPlugSlot
	}
	return r.vmMgr.ENISlotForReconcile(instance, rec.NetworkInterfaceId)
}

// reconcileENI applies matrix rows 1–6 for one ENI record. Returns true when it
// reaped (mutated state toward convergence).
func (r *eniReconciler) reconcileENI(instance *vm.VM, rec *handlers_ec2_vpc.ENIRecord, slot int, live map[int]string) bool {
	present := slot > 0 && live[slot] != ""
	acct := instance.AccountID
	eniID := rec.NetworkInterfaceId

	switch {
	// Rows 5/6: detach in flight — only act once stale so we never race a
	// live detach handler.
	case rec.DetachInFlight || rec.AttachmentStatus == "detaching":
		if !r.isStale(rec) {
			return false
		}
		if present {
			slog.Info("eni-reconciler: replaying interrupted detach", "instanceId", instance.ID, "eniId", eniID, "slot", slot)
			if err := r.vmMgr.HotUnplugENI(context.Background(), instance, eniID, rec.DetachForce); err != nil {
				slog.Warn("eni-reconciler: detach replay failed", "instanceId", instance.ID, "eniId", eniID, "err", err)
			}
		}
		r.finalizeDetached(instance, acct, eniID)
		return true

	// Row 3/4: attach interrupted — finish if the device made it, else roll back.
	case rec.AttachmentStatus == "attaching":
		if !r.isStale(rec) {
			return false
		}
		if present {
			slog.Info("eni-reconciler: completing interrupted attach", "instanceId", instance.ID, "eniId", eniID, "slot", slot)
			r.vmMgr.AdoptENISlot(instance, eniID, slot)
			r.markAttached(acct, eniID, slot)
		} else {
			slog.Info("eni-reconciler: rolling back interrupted attach", "instanceId", instance.ID, "eniId", eniID)
			r.finalizeDetached(instance, acct, eniID)
		}
		return true

	// Row 1: steady attached, device present — re-adopt the slot if the
	// in-memory map was lost across restart. Not a reap.
	case present:
		r.vmMgr.AdoptENISlot(instance, eniID, slot)
		return false

	// Row 2: KV says attached but the guest lost the device — converge to detached.
	case rec.AttachmentStatus == "attached" || rec.HotPlugSlot > 0:
		slog.Info("eni-reconciler: KV attached but device absent, detaching", "instanceId", instance.ID, "eniId", eniID, "slot", slot)
		r.finalizeDetached(instance, acct, eniID)
		return true

	default:
		return false
	}
}

// sweepOrphanDevices removes live hot-plug devices whose slot no KV record
// claims (row 7) — a crashed attach that reached QMP but never wrote KV.
func (r *eniReconciler) sweepOrphanDevices(instance *vm.VM, live map[int]string, claimed map[int]bool) int {
	reaped := 0
	for slot := range live {
		if claimed[slot] {
			continue
		}
		slog.Info("eni-reconciler: removing orphan hot-plug device", "instanceId", instance.ID, "slot", slot)
		if err := r.vmMgr.RemoveENIDeviceBySlot(instance, slot); err != nil {
			slog.Warn("eni-reconciler: orphan device removal failed", "instanceId", instance.ID, "slot", slot, "err", err)
			continue
		}
		reaped++
	}
	return reaped
}

// sweepOrphanSlots releases in-memory slot entries whose ENI has no live KV
// attachment (row 8) — a detach that cleaned KV but not the slot map.
func (r *eniReconciler) sweepOrphanSlots(instance *vm.VM, knownENIs map[string]bool) int {
	reaped := 0
	for _, eniID := range r.vmMgr.ENISlotMapKeys(instance) {
		if knownENIs[eniID] {
			continue
		}
		slog.Info("eni-reconciler: releasing orphan slot entry", "instanceId", instance.ID, "eniId", eniID)
		r.vmMgr.ReleaseENISlot(instance, eniID)
		reaped++
	}
	return reaped
}

// finalizeDetached drives KV + slot + tap to the detached terminal state. Each
// step is idempotent so a re-run after a partial failure converges.
func (r *eniReconciler) finalizeDetached(instance *vm.VM, accountID, eniID string) {
	if err := r.vpc.DetachENI(context.Background(), accountID, eniID); err != nil {
		slog.Warn("eni-reconciler: KV detach failed", "eniId", eniID, "err", err)
	}
	_ = r.vpc.UpdateENI(accountID, eniID, func(rec *handlers_ec2_vpc.ENIRecord) {
		rec.AttachmentStatus = ""
		rec.HotPlugSlot = 0
		rec.DetachInFlight = false
		rec.DetachForce = false
		rec.AttachmentStateAt = time.Now()
	})
	r.vmMgr.ReleaseENISlot(instance, eniID)
	r.vmMgr.CleanupENITap(instance, eniID)
}

// markAttached promotes a record to the attached terminal state.
func (r *eniReconciler) markAttached(accountID, eniID string, slot int) {
	_ = r.vpc.UpdateENI(accountID, eniID, func(rec *handlers_ec2_vpc.ENIRecord) {
		rec.AttachmentStatus = "attached"
		rec.HotPlugSlot = slot
		rec.AttachmentStateAt = time.Now()
	})
}

func (r *eniReconciler) isStale(rec *handlers_ec2_vpc.ENIRecord) bool {
	// A zero timestamp pre-dates the field (Sprint 3d) — treat as stale so
	// records stuck before the upgrade still converge.
	if rec.AttachmentStateAt.IsZero() {
		return true
	}
	return time.Since(rec.AttachmentStateAt) >= r.stale
}
