package elbv2

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/nats-io/nats.go"
)

// KeyDataPath is the data-path reconciler's leader lease in the sync bucket.
const KeyDataPath = "elbv2.datapath"

const (
	lbRepairLeaseTTL      = 60 * time.Second
	lbRepairPassInterval  = 30 * time.Second
	lbRepairMinFailing    = 60 * time.Second
	lbRepairMinVMAge      = 5 * time.Minute
	lbRepairCooldown      = 10 * time.Minute
	lbRepairBudgetWindow  = 24 * time.Hour
	lbRepairBudget        = 3
	lbRepairFleetMinCount = 3

	// A heartbeat that persists only every heartbeatPersistInterval is still
	// fresh for twice that long.
	lbRepairHeartbeatMaxAge = 2 * heartbeatPersistInterval
)

// SystemInstanceRebooter is the only capability the repair path has over a VM.
type SystemInstanceRebooter interface {
	RebootSystemInstance(instanceID string) error
}

var _ SystemInstanceRebooter = SystemInstanceLauncher(nil)

// counts reports whether a repair entry uses up cooldown and budget. Only a
// repair no node received is free.
func (r LBRepair) counts() bool {
	return r.Outcome != RepairUnreachable
}

// repairWindow counts the entries inside window ending at now, and returns the
// newest of them.
func repairWindow(repairs []LBRepair, now time.Time, window time.Duration) (n int, newest time.Time) {
	for _, r := range repairs {
		if !r.counts() || now.Sub(r.At) >= window {
			continue
		}
		n++
		if r.At.After(newest) {
			newest = r.At
		}
	}
	return n, newest
}

// lbRepairReconciler restarts the VM of a load balancer whose data NIC has been
// failing its gateway probe. Only the lease holder acts.
type lbRepairReconciler struct {
	svc      *ELBv2ServiceImpl
	rebooter SystemInstanceRebooter
	isLeader func() bool
	now      func() time.Time

	mu             sync.Mutex
	budgetReported map[string]bool
}

func newLBRepairReconciler(svc *ELBv2ServiceImpl, rebooter SystemInstanceRebooter, isLeader func() bool) *lbRepairReconciler {
	return &lbRepairReconciler{
		svc:            svc,
		rebooter:       rebooter,
		isLeader:       isLeader,
		now:            func() time.Time { return time.Now().UTC() },
		budgetReported: make(map[string]bool),
	}
}

// NewDataPathLease returns the data-path reconciler's leader lease for holder.
func NewDataPathLease(store kvsync.KVStore, holder string) *kvsync.Lease {
	return kvsync.NewLease(store, KeyDataPath, holder, lbRepairLeaseTTL)
}

// StartDataPathReconciler contends for lease and, while holding it, runs the
// repair pass every lbRepairPassInterval until ctx or the service is cancelled.
func (s *ELBv2ServiceImpl) StartDataPathReconciler(ctx context.Context, lease *kvsync.Lease) {
	if s.InstanceLauncher == nil || s.store == nil || lease == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	if s.ctx != nil {
		context.AfterFunc(s.ctx, cancel)
	}
	rec := newLBRepairReconciler(s, s.InstanceLauncher, lease.Held)
	go kvsync.RunElection(ctx, lease, nil, nil)
	go func() {
		defer cancel()
		ticker := time.NewTicker(lbRepairPassInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rec.pass(ctx)
			}
		}
	}()
}

// candidate is a load balancer that is failing and may be repaired.
type lbRepairCandidate struct {
	lb    *LoadBalancerRecord
	rec   *LBDataPathRecord
	mac   string
	nic   *LBNICFailure
	since time.Time
}

// pass runs one repair pass. It starts at most one repair.
func (r *lbRepairReconciler) pass(ctx context.Context) {
	if !r.isLeader() {
		return
	}
	lbs, err := r.svc.store.ListLoadBalancersStrict(ctx)
	if err != nil {
		slog.WarnContext(ctx, "ELBv2 data-path reconciler: failed to list load balancers", "err", err)
		return
	}
	now := r.now()

	var heartbeating, failing int
	var cands []lbRepairCandidate
	live := make(map[string]bool, len(lbs))
	for _, lb := range lbs {
		live[lb.LoadBalancerID] = true
		if lb.State != StateActive || lb.InstanceID == "" || now.Sub(lb.LastHeartbeat) >= lbRepairHeartbeatMaxAge {
			continue
		}
		heartbeating++
		rec, err := r.svc.store.GetLBHealth(ctx, lb.LoadBalancerID)
		if err != nil || rec == nil || rec.Suspended || rec.InstanceID != lb.InstanceID || !rec.failing() {
			continue
		}
		failing++
		c := lbRepairCandidate{lb: lb, rec: rec}
		for mac, nic := range rec.NICs {
			if c.nic == nil || nic.FailingSince.Before(c.since) {
				c.mac, c.nic, c.since = mac, nic, nic.FailingSince
			}
		}
		cands = append(cands, c)
	}
	r.forgetBudgetReports(live)

	if failing >= lbRepairFleetMinCount && failing*2 > heartbeating {
		slog.WarnContext(ctx, "ELBv2 data-path repair suppressed: fleet-wide probe failure",
			"failing_lbs", failing, "heartbeating_lbs", heartbeating)
		return
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].since.Before(cands[j].since) })
	for _, c := range cands {
		if r.eligible(ctx, c, now) && r.repair(ctx, c) {
			return
		}
	}
}

// eligible applies the per-LB conditions that need no write.
func (r *lbRepairReconciler) eligible(ctx context.Context, c lbRepairCandidate, now time.Time) bool {
	if now.Sub(c.since) < lbRepairMinFailing {
		return false
	}
	if now.Sub(vmRunningSince(c.lb, c.rec)) < lbRepairMinVMAge {
		return false
	}
	n, newest := repairWindow(c.rec.Repairs, now, lbRepairBudgetWindow)
	if n >= lbRepairBudget {
		r.reportBudgetSpent(ctx, c, n)
		return false
	}
	r.clearBudgetReport(c.lb.LoadBalancerID)
	return now.Sub(newest) >= lbRepairCooldown || newest.IsZero()
}

// vmRunningSince is when the LB's current VM last came up as far as the control
// plane can tell: its activation, a relaunch, or the last repair.
func vmRunningSince(lb *LoadBalancerRecord, rec *LBDataPathRecord) time.Time {
	since := lb.ActiveSince
	if since.IsZero() {
		since = lb.CreatedAt
	}
	if rec.BoundAt.After(since) {
		since = rec.BoundAt
	}
	for _, e := range rec.Repairs {
		if e.counts() && e.At.After(since) {
			since = e.At
		}
	}
	return since
}

// repair reserves the attempt durably, then dispatches it. It reports whether a
// repair was started, which ends the pass.
func (r *lbRepairReconciler) repair(ctx context.Context, c lbRepairCandidate) bool {
	if !r.isLeader() {
		return false
	}
	lb, err := r.svc.store.GetLoadBalancer(ctx, c.lb.LoadBalancerID)
	if err != nil || lb == nil || lb.State != StateActive || lb.InstanceID != c.lb.InstanceID {
		return false
	}
	now := r.now()
	reason := "data NIC failing gateway probe"
	var attempt int
	reserved := false
	_, err = r.svc.store.UpdateLBHealth(ctx, lb.LoadBalancerID, func(cur *LBDataPathRecord) (bool, error) {
		// The callback reruns after a conflict, so admission is judged by the run
		// that wrote the reservation and nothing earlier.
		reserved, attempt = false, 0
		nic := cur.NICs[c.mac]
		if cur.Suspended || cur.InstanceID != lb.InstanceID || nic == nil || now.Sub(nic.FailingSince) < lbRepairMinFailing {
			return false, nil
		}
		n, newest := repairWindow(cur.Repairs, now, lbRepairBudgetWindow)
		if n >= lbRepairBudget || (!newest.IsZero() && now.Sub(newest) < lbRepairCooldown) {
			return false, nil
		}
		cur.Repairs = append(pruneRepairs(cur.Repairs, now), LBRepair{
			At: now, InstanceID: lb.InstanceID, ENIID: nic.ENIID, Reason: reason, Outcome: RepairReserved,
		})
		attempt = n + 1
		reserved = true
		return true, nil
	})
	if err != nil {
		slog.WarnContext(ctx, "ELBv2 data-path repair: reservation failed", "lbId", lb.LoadBalancerID, "err", err)
		return false
	}
	if !reserved {
		return false
	}

	attrs := repairLogAttrs(lb, c, attempt, reason, now)
	if !r.isLeader() {
		slog.WarnContext(ctx, "ELBv2 data-path repair not dispatched: leadership lost after reservation", attrs...)
		return true
	}
	slog.WarnContext(ctx, "ELBv2 data-path repair started", attrs...)
	outcome := r.dispatch(lb.InstanceID)
	if outcome != "" {
		r.recordOutcome(ctx, lb.LoadBalancerID, now, lb.InstanceID, outcome)
	}
	switch outcome {
	case RepairDispatched:
		slog.InfoContext(ctx, "ELBv2 data-path repair dispatched", attrs...)
	case RepairRefused:
		slog.ErrorContext(ctx, "ELBv2 data-path repair refused by the owning node", attrs...)
	case RepairUnreachable:
		slog.WarnContext(ctx, "ELBv2 data-path repair: owning node unreachable, will retry", attrs...)
	default:
		slog.WarnContext(ctx, "ELBv2 data-path repair: outcome unknown, counted", attrs...)
	}
	return true
}

// dispatch asks the owning node to restart exactly the LB's own VM. It returns
// an empty outcome when the reply was lost, which leaves the entry reserved.
func (r *lbRepairReconciler) dispatch(instanceID string) string {
	err := r.rebooter.RebootSystemInstance(instanceID)
	switch {
	case err == nil:
		return RepairDispatched
	case errors.Is(err, nats.ErrNoResponders), strings.Contains(err.Error(), "no responders"):
		return RepairUnreachable
	case errors.Is(err, systeminstance.ErrSystemInstanceNotFound),
		errors.Is(err, systeminstance.ErrSystemInstanceNotSystem),
		errors.Is(err, systeminstance.ErrSystemInstanceOwnershipUnknown):
		return RepairRefused
	default:
		return ""
	}
}

func (r *lbRepairReconciler) recordOutcome(ctx context.Context, lbID string, at time.Time, instanceID, outcome string) {
	_, err := r.svc.store.UpdateLBHealth(ctx, lbID, func(cur *LBDataPathRecord) (bool, error) {
		for i := range cur.Repairs {
			e := &cur.Repairs[i]
			if e.At.Equal(at) && e.InstanceID == instanceID && e.Outcome == RepairReserved {
				e.Outcome = outcome
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		slog.WarnContext(ctx, "ELBv2 data-path repair: failed to record outcome", "lbId", lbID, "outcome", outcome, "err", err)
	}
}

// pruneRepairs drops entries older than the budget window.
func pruneRepairs(repairs []LBRepair, now time.Time) []LBRepair {
	kept := repairs[:0:0]
	for _, e := range repairs {
		if now.Sub(e.At) < lbRepairBudgetWindow {
			kept = append(kept, e)
		}
	}
	return kept
}

func repairLogAttrs(lb *LoadBalancerRecord, c lbRepairCandidate, attempt int, reason string, now time.Time) []any {
	return []any{
		"lbArn", lb.LoadBalancerArn, "lbId", lb.LoadBalancerID, "instanceId", lb.InstanceID, "nodeId", lb.NodeID,
		"eniId", c.nic.ENIID, "mac", c.mac, "failing_since", c.nic.FailingSince,
		"failing_ms", otelsetup.Millis(now.Sub(c.nic.FailingSince)),
		"consecutive_failures", c.nic.ConsecutiveFailures,
		"rx_packets", c.nic.RxPackets, "tx_packets", c.nic.TxPackets,
		"rx_dropped", c.nic.RxDropped, "tx_dropped", c.nic.TxDropped,
		"repair_attempt", attempt, "reason", reason,
	}
}

func (r *lbRepairReconciler) reportBudgetSpent(ctx context.Context, c lbRepairCandidate, n int) {
	r.mu.Lock()
	already := r.budgetReported[c.lb.LoadBalancerID]
	r.budgetReported[c.lb.LoadBalancerID] = true
	r.mu.Unlock()
	if already {
		return
	}
	slog.ErrorContext(ctx, "ELBv2 data-path repair budget exhausted",
		"lbArn", c.lb.LoadBalancerArn, "lbId", c.lb.LoadBalancerID, "instanceId", c.lb.InstanceID,
		"eniId", c.nic.ENIID, "mac", c.mac, "repairs_24h", n)
}

func (r *lbRepairReconciler) clearBudgetReport(lbID string) {
	r.mu.Lock()
	delete(r.budgetReported, lbID)
	r.mu.Unlock()
}

func (r *lbRepairReconciler) forgetBudgetReports(live map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.budgetReported {
		if !live[id] {
			delete(r.budgetReported, id)
		}
	}
}
