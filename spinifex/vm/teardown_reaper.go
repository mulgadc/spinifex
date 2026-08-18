package vm

import (
	"context"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"log/slog"
	"strings"
	"sync"
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

	mu      sync.Mutex
	backoff map[string]*depBackoff
}

// depBackoff spaces out re-drives of a dependent that keeps failing.
//
// A dependent can fail for a reason no number of retries will fix — a volume
// whose metadata document cannot be reassembled, say. Re-driving it on every
// sweep costs a full object-store read each time, and one such record was
// enough to hold an object-store request slot open continuously and slow every
// other listing on the cluster behind it. The retry still happens, and the
// dependent still stays Failed; it just stops being free.
type depBackoff struct {
	failures int
	next     time.Time
}

// teardownRetryBase and teardownRetryMax bound the backoff. The first failure
// is retried on the next sweep, and the interval doubles from there.
const (
	teardownRetryBase = 30 * time.Second
	teardownRetryMax  = 30 * time.Minute
)

var _ Reaper = (*TerminatedTeardownReaper)(nil)

// NewTerminatedTeardownReaper builds the reaper bound to this Manager's cleaner
// and state store.
func (m *Manager) NewTerminatedTeardownReaper() *TerminatedTeardownReaper {
	return &TerminatedTeardownReaper{m: m, backoff: make(map[string]*depBackoff)}
}

// dueForRetry reports whether a dependent may be re-driven on this sweep. It is
// in-memory and node-local: a restart simply retries everything once more.
func (r *TerminatedTeardownReaper) dueForRetry(instanceID, dep string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.backoff[instanceID+"/"+dep]
	return b == nil || !time.Now().Before(b.next)
}

// recordRetry advances or clears a dependent's backoff from its result.
func (r *TerminatedTeardownReaper) recordRetry(instanceID, dep string, state TeardownState) {
	key := instanceID + "/" + dep
	r.mu.Lock()
	defer r.mu.Unlock()
	if state == TeardownDone {
		delete(r.backoff, key)
		return
	}
	b := r.backoff[key]
	if b == nil {
		b = &depBackoff{}
		r.backoff[key] = b
	}
	b.failures++
	wait := teardownRetryBase << min(b.failures-1, 16)
	if wait > teardownRetryMax || wait <= 0 {
		wait = teardownRetryMax
	}
	b.next = time.Now().Add(wait)
	slog.Warn("vm/gc: teardown dependent failed, backing off",
		"instanceId", instanceID, "dependent", dep, "failures", b.failures, "retry_in_ms", otelsetup.Millis(wait))
}

// forgetInstance drops an instance's backoff once its record is gone, so the
// map cannot grow without bound across the life of the process.
func (r *TerminatedTeardownReaper) forgetInstance(instanceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.backoff {
		if strings.HasPrefix(key, instanceID+"/") {
			delete(r.backoff, key)
		}
	}
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
	retry := func(dep string, run func() error) {
		if !outstanding(v, dep) || !r.dueForRetry(v.ID, dep) {
			return
		}
		state := resultState(run())
		results[dep] = state
		r.recordRetry(v.ID, dep, state)
	}

	retry(TeardownVolumes, func() error { return c.DeleteVolumes(v) })
	retry(TeardownGPU, func() error { return c.ReleaseGPU(v) })
	retry(TeardownPlacement, func() error { return c.RemoveFromPlacementGroup(v) })

	// NAT: re-publishes vpc.delete-nat + frees the IPAM slot. The cluster
	// reconciler heals the dataplane NAT rule.
	retry(TeardownNAT, func() error { return c.ReleasePublicIP(v) })

	// ENI delete + OVN: deleting the ENI KV record turns its LSP into an orphan
	// the cluster reconcile prune reaps, so a successful ENI delete completes
	// both eni and ovn. Both share the ENI's backoff because one call decides
	// them, and it is gated on whichever of the two is due.
	if (outstanding(v, TeardownENI) || outstanding(v, TeardownOVN)) &&
		(r.dueForRetry(v.ID, TeardownENI) || r.dueForRetry(v.ID, TeardownOVN)) {
		eniErr := c.DetachAndDeleteENI(v)
		results[TeardownENI] = resultState(eniErr)
		results[TeardownOVN] = resultState(eniErr)
		r.recordRetry(v.ID, TeardownENI, results[TeardownENI])
		r.recordRetry(v.ID, TeardownOVN, results[TeardownOVN])
	}

	return results
}

func (r *TerminatedTeardownReaper) purge(v *VM) bool {
	if err := r.m.deps.StateStore.DeleteTerminatedInstance(v.ID); err != nil {
		slog.Warn("vm/gc: failed to purge completed terminated record",
			"instanceId", v.ID, "err", err)
		return false
	}
	r.forgetInstance(v.ID)
	slog.Info("vm/gc: purged terminated record, teardown complete", "instanceId", v.ID)
	return true
}

// outstanding reports whether a teardown dependency is tracked and not yet done.
func outstanding(v *VM, dep string) bool {
	state, ok := v.Teardown[dep]
	return ok && TeardownState(state) != TeardownDone
}
