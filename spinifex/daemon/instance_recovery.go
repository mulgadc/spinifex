package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
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

// recoveryLeaseBackoffBase and recoveryLeaseBackoffMax are the same bounds for
// the one failure that clears on a clock rather than on somebody fixing
// something: the node this instance was taken from still holds its volume
// lease, and will stop holding it within a TTL. Retrying that on the general
// backoff would spend minutes waiting for something already over, so it is
// faster and it is capped near two TTLs — past which the cause is no longer the
// one being waited for, and the attempt budget should be spent finding out.
const (
	recoveryLeaseBackoffBase = 15 * time.Second
	recoveryLeaseBackoffMax  = 90 * time.Second
)

// recoveryMaxAttempts is how many times one node tries an instance before it
// stops and says so.
//
// Retrying forever was the previous behaviour and it is the worse failure. The
// instance stays observed-running on a node that is gone, nothing is visible to
// the customer beyond an instance that does not answer, and no operator is ever
// told. A budget converts that into a stopped instance carrying the reason, on
// the same path AWS leaves a host-failed instance on. At these delays the budget
// is roughly an hour and a half of trying, so anything that was going to clear
// has had far longer than the clocks it depends on.
const recoveryMaxAttempts = 12

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
//
// There is no setting for this. An instance whose host is gone is down either
// way, so the only thing a switch could buy is leaving it down, and a cluster
// configured into that state by accident is a worse outcome than any this
// recovers from. The conditions below are what make it inert, and each is a
// fact about the cluster rather than a choice about it.
func (d *Daemon) startInstanceRecovery() {
	if d.clusterConfig == nil {
		slog.Warn("Instance recovery not started: no cluster config")
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

	startedAt   time.Time
	settledOnce bool
	staleOnce   map[string]struct{}
	backoff     map[string]recoveryBackoff
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
	state := r.peerState(ctx)
	if !r.settled(state) || !r.canHostRecoveries(ctx) {
		return
	}

	records, err := r.daemon.jsManager.ListInstanceRecords()
	if err != nil {
		slog.Warn("Instance recovery could not read the instance records", "err", err)
		return
	}

	for _, candidate := range r.selectCandidates(records, state) {
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
// serves the first without spending the cluster on the second, and the bounds it
// doubles between come from the fault, which is the other half of the same
// distinction.
//
// Every fault counts against the same budget. A class that did not count would
// be the old unbounded behaviour under a narrower condition, and the conditions
// this can read are not precise enough to be trusted with that.
func (r *instanceRecovery) deferRetry(id string, fault recoveryFault, now time.Time) int {
	base, limit := fault.backoff()
	held := r.backoff[id]
	held.attempts++
	delay := min(base<<min(held.attempts-1, 16), limit)
	held.until = now.Add(delay)
	r.backoff[id] = held
	return held.attempts
}

// recoveryFault is what a failed attempt says about the next one, and it is
// deliberately a small set: how long to wait, and what to tell the customer if
// the budget runs out.
//
// There is no terminal class, because nothing on this path can prove one. The
// launch reports a capacity refusal and a lease refusal distinguishably and
// collapses everything else into ServerInternal, so a fault named "will never
// succeed" would be a guess dressed as a verdict — and the guess that abandons a
// recoverable instance is the expensive one. The budget bounds the unknown case
// instead, which needs no such claim.
type recoveryFault int

const (
	// recoveryFaultLeaseHeld: the node this was taken from still holds a volume
	// lease. Ordinary, expected, and over within a TTL.
	recoveryFaultLeaseHeld recoveryFault = iota

	// recoveryFaultCapacity: this node has no room for the instance. Another
	// survivor may, and each derives its own order, so they all get a turn.
	recoveryFaultCapacity

	// recoveryFaultUnknown: everything else, which is most of it.
	recoveryFaultUnknown
)

func (f recoveryFault) String() string {
	switch f {
	case recoveryFaultLeaseHeld:
		return "volume_lease_held"
	case recoveryFaultCapacity:
		return "insufficient_capacity"
	default:
		return "unknown"
	}
}

// backoff is the delay range for this fault.
func (f recoveryFault) backoff() (base, limit time.Duration) {
	if f == recoveryFaultLeaseHeld {
		return recoveryLeaseBackoffBase, recoveryLeaseBackoffMax
	}
	return recoveryBackoffBase, recoveryBackoffMax
}

// stateReasonCode is what an abandoned instance carries for this fault. AWS has
// a real code for a capacity refusal, so that one is not invented; the rest have
// no AWS equivalent and get the code that says which subsystem gave up, distinct
// from the one a node's own restart uses.
func (f recoveryFault) stateReasonCode() string {
	if f == recoveryFaultCapacity {
		return "Server.InsufficientInstanceCapacity"
	}
	return "Server.HostRecoveryFailed"
}

// classifyRecoveryFault reads what the launch refused with.
func classifyRecoveryFault(err error) recoveryFault {
	if errors.Is(err, handlers_ec2_instance.ErrVolumeHeldElsewhere) {
		return recoveryFaultLeaseHeld
	}
	if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorInsufficientInstanceCapacity {
		return recoveryFaultCapacity
	}
	return recoveryFaultUnknown
}

// settled reports whether this daemon has been up long enough to believe what
// the heartbeats say about its peers.
//
// It latches, and that is the point. The question is whether this node is still
// coming up alongside its peers, which is answered once and for good the first
// time it sees them all live. Asked afresh every pass it would be a different
// question — whether the peers are live right now — and would answer no for
// exactly the failure the reconciler exists to handle.
//
// A node that never comes back is covered by the window expiring, which is the
// genuine-failure case with nothing to compare against.
func (r *instanceRecovery) settled(state func(string) instancecache.NodeState) bool {
	if r.settledOnce {
		return true
	}
	if time.Since(r.startedAt) >= recoverySettleWindow {
		r.settledOnce = true
		return true
	}
	for name := range r.daemon.clusterConfig.Nodes {
		if name == r.daemon.node {
			continue
		}
		if state(name) != instancecache.NodeLive {
			return false
		}
	}
	r.settledOnce = true
	slog.Info("Instance recovery has seen every peer live and is now active", "node", r.daemon.node)
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

// peerState is the liveness view as a function, so everything that decides on
// it stays pure over the answer and testable without a cluster.
func (r *instanceRecovery) peerState(ctx context.Context) func(string) instancecache.NodeState {
	return func(node string) instancecache.NodeState {
		return r.liveness.State(ctx, node)
	}
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
		fault := classifyRecoveryFault(err)
		attempts := r.deferRetry(id, fault, time.Now())
		slog.Warn("Instance recovery attempt did not complete",
			"instanceId", id, "from", candidate.from, "fault", fault.String(),
			"attempts", attempts, "budget", recoveryMaxAttempts, "err", err)
		if attempts >= recoveryMaxAttempts {
			r.abandon(id, candidate.from, fault, err, attempts)
		}
		return
	}

	delete(r.staleOnce, id)
	delete(r.backoff, id)

	// The guest is running here and the cloud underneath still delivers its
	// public address to the node that died, so this is the last thing between a
	// recovered instance and a reachable one. It runs in line rather than waiting
	// for the affinity ticker, and a guest whose address does not arrive is
	// marked impaired by the pass rather than reported healthy.
	r.daemon.ClaimOCIAddresses(ctx)

	slog.Info("Recovered an instance onto this node",
		"instanceId", id, "from", candidate.from, "node", r.daemon.node)
}

// abandon stops trying and leaves the instance stopped with the reason it
// stopped for, which is the only outcome here that is visible to the customer.
//
// This is the same place AWS leaves an instance whose host failed and could not
// be brought back: stopped, carrying a StateReason, one StartInstances away from
// running. So the customer's retry is the ordinary API call on whichever node
// has room, and nothing here needs an operator-only path to undo it.
//
// A record another node has since claimed and launched is not touched, and a
// write that does not land leaves the backoff in place — the next pass tries the
// launch again rather than the give-up, which is the safer of the two to repeat.
func (r *instanceRecovery) abandon(id, from string, fault recoveryFault, cause error, attempts int) {
	reason := fmt.Sprintf("recovery onto %s failed %d times, last: %v", r.daemon.node, attempts, cause)

	abandoned, err := r.daemon.jsManager.AbandonRecovery(id, from, fault.stateReasonCode(), reason)
	if err != nil {
		slog.Error("Instance recovery could not record that it has given up",
			"instanceId", id, "from", from, "err", err)
		return
	}
	if !abandoned {
		slog.Info("Instance recovery gave up on an instance that is no longer its to give up on",
			"instanceId", id, "from", from)
		delete(r.backoff, id)
		delete(r.staleOnce, id)
		return
	}

	delete(r.backoff, id)
	delete(r.staleOnce, id)
	slog.Error("Instance recovery has given up: the instance is stopped and needs a start",
		"instanceId", id, "from", from, "node", r.daemon.node, "attempts", attempts,
		"fault", fault.String(), "code", fault.stateReasonCode(), "err", cause)
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
