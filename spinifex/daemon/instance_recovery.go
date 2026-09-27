package daemon

import (
	"cmp"
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// recoveryPassInterval is the gap between passes. It is well under the volume
// lease TTL, so a candidate refused because the old owner's lease has not
// expired is retried within that window rather than after it.
const recoveryPassInterval = 15 * time.Second

// recoverySettleWindow is how long after this daemon started recovery stays
// inert when peers are still missing.
//
// It is the whole of the coordinated-shutdown suppression. The documented
// deploy path stops every node and then starts them, so the first one back
// finds every peer stale and every guest on them a candidate. Waiting until
// the peers are seen, or until this window expires, turns that into nothing
// happening. It is also why the window is generous: the cost of waiting is a
// slower recovery after a genuine failure, and the cost of not waiting is
// recovering guests from nodes that are merely still booting.
const recoverySettleWindow = 5 * time.Minute

// recoveryMinimumNodes is the cluster size below which recovery cannot mean
// anything: on one or two nodes there is either nowhere to move a guest to, or
// no KV quorum left to move it with.
const recoveryMinimumNodes = 3

// recoveryBackoffBase and recoveryBackoffMax bound how often one node retries
// an instance whose launch failed.
//
// Some failures are not the node's to fix. A guest whose object store is
// refusing reads fails to mount identically on every survivor, and without a
// backoff each of them would claim it, fail, hand it back and claim it again
// every pass. Backing off turns that into a slow probe that costs nothing and
// still finishes the recovery the moment the store returns.
const (
	recoveryBackoffBase = 30 * time.Second
	recoveryBackoffMax  = 15 * time.Minute
)

// recoveryCandidate is one instance this node could take over, and why.
type recoveryCandidate struct {
	record *vm.InstanceRecord
	from   string
}

// startInstanceRecovery runs the recovery reconciler on this daemon.
//
// Every node runs the same loop with no leader between them. A leader would
// add failover state and no safety: the claim is a CAS on the instance record,
// so several nodes racing one instance already resolve to one winner.
func (d *Daemon) startInstanceRecovery() {
	if !d.config.Recovery.Enabled {
		slog.Info("Instance recovery is disabled", "node", d.node)
		return
	}
	if d.jsManager == nil || d.instanceService == nil {
		slog.Warn("Instance recovery not started: JetStream or the instance service is unavailable")
		return
	}
	if n := len(d.clusterConfig.Nodes); n < recoveryMinimumNodes {
		slog.Info("Instance recovery is inert on a cluster this small",
			"node", d.node, "nodes", n, "minimum", recoveryMinimumNodes)
		return
	}

	liveness := d.newLiveness()
	if liveness == nil {
		slog.Warn("Instance recovery not started: no liveness view")
		return
	}
	rec := &instanceRecovery{
		daemon:    d,
		liveness:  liveness,
		startedAt: time.Now(),
		staleOnce: map[string]struct{}{},
		backoff:   map[string]recoveryBackoff{},
	}

	d.shutdownWg.Go(func() {
		ticker := time.NewTicker(recoveryPassInterval)
		defer ticker.Stop()
		for {
			select {
			case <-d.ctx.Done():
				slog.Info("Instance recovery stopping")
				return
			case <-ticker.C:
				rec.pass(d.ctx)
			}
		}
	})

	slog.Info("Instance recovery started", "node", d.node,
		"interval_ms", otelsetup.Millis(recoveryPassInterval),
		"settle_ms", otelsetup.Millis(recoverySettleWindow))
}

// instanceRecovery is one daemon's recovery loop. The only state it keeps is
// which instances were already seen stale, which is what makes a second pass a
// condition rather than a duplicate of the first.
type instanceRecovery struct {
	daemon   *Daemon
	liveness *instancecache.Liveness

	startedAt time.Time
	staleOnce map[string]struct{}
	backoff   map[string]recoveryBackoff
}

// recoveryBackoff is how long this node has agreed to leave an instance alone
// after a launch that did not complete, and how many attempts it has made.
type recoveryBackoff struct {
	until    time.Time
	attempts int
}

// pass derives the work from the records once, then attempts what this node can
// take. Nothing is stored between passes but the stale-once set, so a daemon
// that restarts derives exactly the same work again.
func (r *instanceRecovery) pass(ctx context.Context) {
	if !r.settled(ctx) || !r.canHostRecoveries(ctx) {
		return
	}

	records, err := r.daemon.jsManager.ListInstanceRecords()
	if err != nil {
		slog.Warn("Instance recovery could not read the instance records", "err", err)
		return
	}

	for _, candidate := range r.candidates(ctx, records) {
		if ctx.Err() != nil {
			return
		}
		if r.backingOff(candidate.record.Metadata.Name, time.Now()) {
			continue
		}
		r.attempt(ctx, candidate)
	}
}

// canHostRecoveries reports whether this node's own object store is answering,
// which is the difference between the two failures that look alike from here.
//
// A node cannot mount a volume its object store will not serve, so claiming one
// would take an instance off its dead owner only to fail, and hold it away from
// a survivor that could have run it. Standing down separates the cases without
// anything having to diagnose them: if the store is broken only here, the other
// survivors carry on and the instance moves; if it is broken everywhere, every
// node stands down and the instance stays where it is, which is the honest
// answer because no node could have run it.
func (r *instanceRecovery) canHostRecoveries(ctx context.Context) bool {
	verdict := probePredastore(ctx, r.daemon)
	if verdict == predastoreHealthOK {
		return true
	}
	slog.Warn("Instance recovery is standing down: this node's object store is not answering",
		"node", r.daemon.node, "predastore", verdict)
	return false
}

// backingOff reports whether this node has agreed to leave an instance alone
// for now, and forgets the agreement once it has lapsed.
func (r *instanceRecovery) backingOff(id string, now time.Time) bool {
	held, ok := r.backoff[id]
	if !ok {
		return false
	}
	return now.Before(held.until)
}

// deferRetry backs this node off an instance whose launch did not complete, and
// reports how many attempts it has now made.
//
// The delay doubles per attempt because the two reasons a launch fails want
// opposite things. A volume lease the old owner has not released yet clears
// within one TTL and wants a prompt retry; an object store refusing reads clears
// when an operator fixes it and wants no traffic at all in the meantime. Doubling
// serves the first without spending the cluster on the second.
func (r *instanceRecovery) deferRetry(id string, now time.Time) int {
	held := r.backoff[id]
	held.attempts++
	delay := min(recoveryBackoffBase<<min(held.attempts-1, 16), recoveryBackoffMax)
	held.until = now.Add(delay)
	r.backoff[id] = held
	return held.attempts
}

// settled reports whether this daemon has been up long enough to believe what
// the heartbeats say about its peers.
//
// A peer seen live once is enough: the question is whether the cluster is
// mid-restart, not whether every node is healthy now. A node that never comes
// back is covered by the window expiring, which is the genuine-failure case.
func (r *instanceRecovery) settled(ctx context.Context) bool {
	if time.Since(r.startedAt) >= recoverySettleWindow {
		return true
	}
	for name := range r.daemon.clusterConfig.Nodes {
		if name == r.daemon.node {
			continue
		}
		if r.liveness.State(ctx, name) != instancecache.NodeLive {
			return false
		}
	}
	return true
}

// selectCandidates is the whole of the policy, and is pure over what it is
// given so the matrix is testable without a cluster.
//
// NodeUnknown is never a candidate. An unreadable heartbeat is not evidence
// that a node is gone, and treating it as one turns a KV outage into a
// cluster-wide relaunch.
func (r *instanceRecovery) selectCandidates(records []*vm.InstanceRecord, state func(string) instancecache.NodeState) []recoveryCandidate {
	var out []recoveryCandidate
	seen := map[string]struct{}{}

	for _, record := range records {
		owner := record.Status.LastNode
		if owner == "" || owner == r.daemon.node || !recoverable(record) {
			continue
		}
		if state(owner) != instancecache.NodeStale {
			continue
		}

		// Two consecutive passes, because liveness is computed from the
		// timestamp the owner wrote into its own heartbeat rather than from
		// when the store received it. One sample is therefore one node's clock;
		// two are a condition that outlived a pass.
		id := record.Metadata.Name
		seen[id] = struct{}{}
		if _, twice := r.staleOnce[id]; !twice {
			continue
		}
		out = append(out, recoveryCandidate{record: record, from: owner})
	}

	for id := range r.staleOnce {
		if _, still := seen[id]; !still {
			delete(r.staleOnce, id)
		}
	}
	for id := range seen {
		r.staleOnce[id] = struct{}{}
	}

	// Every survivor derives the same candidates, so a shared order would have
	// them all race the same instance first and lose to the same winner. Hashing
	// the pair gives each node its own order for free: no coordination, no
	// randomness to make a failure unreproducible, and the recoveries spread.
	slices.SortFunc(out, func(a, b recoveryCandidate) int {
		return cmp.Compare(r.preference(a.record.Metadata.Name), r.preference(b.record.Metadata.Name))
	})
	return out
}

// preference orders one node's candidates differently from every other node's.
func (r *instanceRecovery) preference(instanceID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(r.daemon.node))
	_, _ = h.Write([]byte(instanceID))
	return h.Sum64()
}

// candidates is selectCandidates against the live liveness view.
func (r *instanceRecovery) candidates(ctx context.Context, records []*vm.InstanceRecord) []recoveryCandidate {
	return r.selectCandidates(records, func(node string) instancecache.NodeState {
		return r.liveness.State(ctx, node)
	})
}

// attempt claims one instance and launches it here.
//
// Claim before admission, not after: the claim is what stops two survivors
// doing the same expensive setup, and a node that turns out not to fit hands
// the record straight back.
func (r *instanceRecovery) attempt(ctx context.Context, candidate recoveryCandidate) {
	id := candidate.record.Metadata.Name

	instance, err := r.daemon.jsManager.ClaimRecoverableInstance(id, candidate.from, r.daemon.node)
	if err != nil {
		if !errors.Is(err, vm.ErrRecoveryClaimLost) {
			slog.Warn("Instance recovery could not claim an instance", "instanceId", id, "err", err)
		}
		return
	}

	undo := func() {
		if err := r.daemon.jsManager.ReleaseRecoveredInstance(id, r.daemon.node, candidate.from); err != nil {
			slog.Error("Instance recovery could not hand a claimed instance back",
				"instanceId", id, "to", candidate.from, "err", err)
		}
	}

	slog.Info("Recovering an instance from a node that stopped heartbeating",
		"instanceId", id, "from", candidate.from, "to", r.daemon.node)

	if err := r.daemon.instanceService.RecoverInstance(ctx, instance, undo); err != nil {
		// Expected while the old owner's volume lease is still valid: its own
		// self-fence has to run before anyone else may open the volume.
		attempts := r.deferRetry(id, time.Now())
		slog.Warn("Instance recovery attempt did not complete",
			"instanceId", id, "from", candidate.from, "attempts", attempts, "err", err)
		return
	}

	delete(r.staleOnce, id)
	delete(r.backoff, id)
	slog.Info("Recovered an instance onto this node",
		"instanceId", id, "from", candidate.from, "node", r.daemon.node)
}

// newLiveness opens the daemon's own read-only view of the heartbeat store.
// The gateway builds one of these too; they share the bucket and neither owns
// it, so a second reader costs one memoised list per pass.
func (d *Daemon) newLiveness() *instancecache.Liveness {
	if d.jsManager == nil || d.jsManager.js == nil {
		return nil
	}
	return instancecache.NewLiveness(d.jsManager.js, clusterStateConfig(d.jsManager.replicas))
}

// forgetSupersededInstances drops any local copy of an instance the records say
// another node now runs, before restore can relaunch it.
//
// A returning node loads its own state file, and an instance recovered while it
// was away is still in there. Nothing else stops it starting a second copy: the
// cluster's view of this node no longer lists that instance, but the local file
// is folded in on top of it, and the running-set writer would then stamp this
// node back over the winner.
//
// A record that cannot be read leaves the instance alone. An unreadable store
// is not evidence that an instance moved, and the volume lease refuses a second
// writer either way.
func (d *Daemon) forgetSupersededInstances() {
	if d.jsManager == nil {
		return
	}

	for _, instance := range d.vmMgr.Snapshot() {
		record, err := d.jsManager.LoadInstanceRecord(instance.ID)
		if err != nil {
			slog.Warn("Could not tell whether an instance moved while this node was away",
				"instanceId", instance.ID, "err", err)
			continue
		}
		if record == nil || record.Status.LastNode == "" || record.Status.LastNode == d.node {
			continue
		}

		slog.Info("An instance here is owned by another node now; dropping the local copy",
			"instanceId", instance.ID, "owner", record.Status.LastNode, "node", d.node)
		d.vmMgr.ForgetSuperseded(instance)
	}
}
