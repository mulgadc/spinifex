package daemon

import (
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// recordRepairInterval bounds how long an instance record can stay unpublished.
//
// Record writes are best-effort and a failed one is otherwise retried only by
// the next state change, so an instance that launches, fails its write and then
// simply runs would have no record for as long as it lived. This sweep is the
// bound on that window, and the bound is the point of it.
const recordRepairInterval = 60 * time.Second

// startRecordRepair republishes instance records whose write failed, on a
// ticker, independent of state change. The goroutine exits when d.ctx is
// cancelled.
//
// It does not fire immediately: the daemon writes its state during startup, and
// a sweep racing that would reconcile against a set still being populated.
func (d *Daemon) startRecordRepair() {
	if d.jsManager == nil {
		slog.Warn("JetStream not initialized, skipping instance record repair")
		return
	}

	interval := d.recordRepairInterval
	if interval <= 0 {
		interval = recordRepairInterval
	}

	d.shutdownWg.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-d.ctx.Done():
				slog.Info("Instance record repair stopping")
				return
			case <-ticker.C:
				d.repairInstanceRecords()
			}
		}
	})

	slog.Info("Instance record repair started", "interval_ms", otelsetup.Millis(interval))
}

// repairInstanceRecords runs one sweep. It reconciles the node's live instances
// against the digest of what it has already published, so a node with nothing
// outstanding writes nothing and the cost tracks failures rather than instance
// count.
//
// Deliberately not routed through persistState: the local state file is already
// correct and rewriting it would advance the state revision for no reason. Only
// the record half needs repairing.
func (d *Daemon) repairInstanceRecords() {
	var result RunningSetResult
	d.vmMgr.View(func(vms map[string]*vm.VM) {
		result = d.jsManager.WriteRunningSet(d.node, d.config.AZ, vms)
	})

	if result.Written == 0 && result.Retired == 0 && result.Failed == 0 {
		return
	}
	// Only a sweep that found work says anything. Any output here means a record
	// write had previously failed, so silence is the healthy state.
	slog.Info("Instance record repair swept", "node", d.node,
		"written", result.Written, "retired", result.Retired, "failed", result.Failed)
}
