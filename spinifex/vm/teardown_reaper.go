package vm

import (
	"context"
	"fmt"
	"log/slog"
)

// TerminatedTeardownReaper completes interrupted instance teardown (ADR-0003
// §1/§3). It scans the terminated KV bucket for records this node owns whose
// per-dependent teardown is not all `done`, re-drives each outstanding
// dependent through the now-idempotent cleaner, and purges the terminated
// record once every dependent reaches `done`. A node-down mid-cascade leaves
// dependents `pending`/`failed`; this reaper finishes them rather than
// abandoning them.
type TerminatedTeardownReaper struct {
	m *Manager
}

var _ Reaper = (*TerminatedTeardownReaper)(nil)

// NewTerminatedTeardownReaper builds the reaper bound to this Manager's cleaner
// and state store.
func (m *Manager) NewTerminatedTeardownReaper() *TerminatedTeardownReaper {
	return &TerminatedTeardownReaper{m: m}
}

func (r *TerminatedTeardownReaper) Class() string      { return "instance-teardown" }
func (r *TerminatedTeardownReaper) Scope() ReaperScope { return ScopeNodeLocal }

// Sweep finishes outstanding teardown for this node's terminated instances.
// Records already complete on arrival are left to the bucket's TTL (preserving
// describe-visibility); only records this sweep brings to completion are purged.
func (r *TerminatedTeardownReaper) Sweep(context.Context) (int, error) {
	if r.m.deps.StateStore == nil {
		return 0, nil
	}
	terminated, err := r.m.deps.StateStore.ListTerminatedInstances()
	if err != nil {
		return 0, fmt.Errorf("list terminated instances: %w", err)
	}

	reaped := 0
	for _, v := range terminated {
		if v.LastNode != "" && v.LastNode != r.m.deps.NodeID {
			continue // home-node owns its node-local teardown
		}
		if v.TeardownComplete() {
			continue // already done on arrival: leave to the 1h bucket TTL
		}

		r.retryOutstanding(v)

		if v.TeardownComplete() {
			if r.purge(v) {
				reaped++
			}
			continue
		}
		if err := r.m.deps.StateStore.WriteTerminatedInstance(v.ID, v); err != nil {
			slog.Warn("vm/gc: failed to persist advanced teardown marks",
				"instanceId", v.ID, "err", err)
		}
	}
	return reaped, nil
}

// retryOutstanding re-drives every not-`done` dependent through the idempotent
// cleaner and re-stamps the result. The cleaner calls are idempotent (absent →
// success), so a successful re-drive confirms the Spinifex-side teardown; the
// dataplane object (OVN LSP, NAT rule) is pruned by the cluster reconciler.
func (r *TerminatedTeardownReaper) retryOutstanding(v *VM) {
	c := r.m.deps.InstanceCleaner
	if c == nil {
		return
	}

	if outstanding(v, TeardownVolumes) {
		r.m.markTeardownResult(v, TeardownVolumes, c.DeleteVolumes(v))
	}
	if outstanding(v, TeardownGPU) {
		r.m.markTeardownResult(v, TeardownGPU, c.ReleaseGPU(v))
	}
	if outstanding(v, TeardownPlacement) {
		r.m.markTeardownResult(v, TeardownPlacement, c.RemoveFromPlacementGroup(v))
	}

	// NAT: re-publishes vpc.delete-nat + frees the IPAM slot. The cluster
	// reconciler heals the dataplane NAT rule.
	if outstanding(v, TeardownNAT) {
		r.m.markTeardownResult(v, TeardownNAT, c.ReleasePublicIP(v))
	}

	// ENI delete + OVN: deleting the ENI KV record turns its LSP into an orphan
	// the cluster reconcile prune reaps, so a successful ENI delete completes
	// both eni and ovn.
	if outstanding(v, TeardownENI) || outstanding(v, TeardownOVN) {
		eniErr := c.DetachAndDeleteENI(v)
		r.m.markTeardownResult(v, TeardownENI, eniErr)
		r.m.markTeardownResult(v, TeardownOVN, eniErr)
	}
}

func (r *TerminatedTeardownReaper) purge(v *VM) bool {
	if err := r.m.deps.StateStore.DeleteTerminatedInstance(v.ID); err != nil {
		slog.Warn("vm/gc: failed to purge completed terminated record",
			"instanceId", v.ID, "err", err)
		return false
	}
	slog.Info("vm/gc: purged terminated record, teardown complete", "instanceId", v.ID)
	return true
}

// outstanding reports whether a teardown dependency is tracked and not yet done.
func outstanding(v *VM, dep string) bool {
	state, ok := v.Teardown[dep]
	return ok && TeardownState(state) != TeardownDone
}
