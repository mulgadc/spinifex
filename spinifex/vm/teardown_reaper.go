package vm

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// terminatedVisibilityWindow is how long a freshly terminated instance stays
// describable before the GC backstop may reclaim its record early. It exceeds
// the e2e terminate-then-poll budget and stays under the bucket's 1h TTL, which
// bounds visibility regardless. Records completed within the window are left to
// the TTL (AWS keeps terminated instances describable ~1h); only records already
// older than the window when the GC finishes their teardown are purged inline.
const terminatedVisibilityWindow = 15 * time.Minute

// TerminatedTeardownReaper completes interrupted instance teardown (ADR-0003
// §1/§3). It scans the terminated KV bucket for records this node owns whose
// per-dependent teardown is not all `done`, re-drives each outstanding
// dependent through the now-idempotent cleaner, and purges the terminated
// record once every dependent reaches `done` and its describe-visibility window
// has elapsed. A node-down mid-cascade leaves dependents `pending`/`failed`;
// this reaper finishes them rather than abandoning them.
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
// A completed record is purged only once it is older than the visibility window;
// fresher ones (and any already complete on arrival) are left to the bucket's 1h
// TTL so a just-terminated instance stays describable, matching AWS semantics.
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

		results := r.retryOutstanding(v)
		for dep, state := range results {
			if v.Teardown == nil {
				v.Teardown = make(map[string]string)
			}
			v.Teardown[dep] = string(state)
		}

		if v.TeardownComplete() && time.Since(v.TerminatedAt) >= terminatedVisibilityWindow {
			if r.purge(v) {
				reaped++
			}
			continue
		}
		// Either still incomplete (retry next sweep), or complete but inside the
		// describe-visibility window: merge this sweep's marks into whatever is
		// currently in KV via CAS, so a concurrent writer advancing a different
		// dependent for the same record isn't clobbered by our local snapshot.
		if len(results) > 0 {
			if _, err := r.m.deps.StateStore.UpdateTerminatedInstance(v.ID, func(fresh *VM) {
				if fresh.Teardown == nil {
					fresh.Teardown = make(map[string]string)
				}
				for dep, state := range results {
					fresh.Teardown[dep] = string(state)
				}
			}); err != nil {
				slog.Warn("vm/gc: failed to persist advanced teardown marks",
					"instanceId", v.ID, "err", err)
			}
		}
	}
	return reaped, nil
}

// retryOutstanding re-drives every not-`done` dependent through the idempotent
// cleaner and returns the dep→result map for whatever was retried. The cleaner
// calls are idempotent (absent → success), so a successful re-drive confirms
// the Spinifex-side teardown; the dataplane object (OVN LSP, NAT rule) is
// pruned by the cluster reconciler. Pure w.r.t. KV/local state — callers merge
// the results atomically via StateStore.UpdateTerminatedInstance so the cleaner
// calls (which have real side effects) never run twice for a single CAS retry.
func (r *TerminatedTeardownReaper) retryOutstanding(v *VM) map[string]TeardownState {
	c := r.m.deps.InstanceCleaner
	if c == nil {
		return nil
	}

	results := make(map[string]TeardownState)

	if outstanding(v, TeardownVolumes) {
		results[TeardownVolumes] = resultState(c.DeleteVolumes(v))
	}
	if outstanding(v, TeardownGPU) {
		results[TeardownGPU] = resultState(c.ReleaseGPU(v))
	}
	if outstanding(v, TeardownPlacement) {
		results[TeardownPlacement] = resultState(c.RemoveFromPlacementGroup(v))
	}

	// NAT: re-publishes vpc.delete-nat + frees the IPAM slot. The cluster
	// reconciler heals the dataplane NAT rule.
	if outstanding(v, TeardownNAT) {
		results[TeardownNAT] = resultState(c.ReleasePublicIP(v))
	}

	// ENI delete + OVN: deleting the ENI KV record turns its LSP into an orphan
	// the cluster reconcile prune reaps, so a successful ENI delete completes
	// both eni and ovn.
	if outstanding(v, TeardownENI) || outstanding(v, TeardownOVN) {
		eniErr := c.DetachAndDeleteENI(v)
		results[TeardownENI] = resultState(eniErr)
		results[TeardownOVN] = resultState(eniErr)
	}

	return results
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
