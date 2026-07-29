package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// Lease and sweep timing. The holder refreshes well inside the bucket's 60s TTL,
// so a leader that dies is replaced within one TTL rather than one refresh.
const (
	reconcilerLeaderKey = "reconciler"
	leaseRefresh        = 20 * time.Second
	reconcileInterval   = 15 * time.Second

	// How long a creating instance may go without a healthy heartbeat before it
	// is called failed, unless the config overrides it. It has to cover a cold
	// boot plus initdb on the slowest class, so it is deliberately generous — a
	// false failed is worse than a slow one, since the customer sees a broken
	// create either way but only the false one is wrong.
	defaultBootstrapTimeout = 20 * time.Minute

	// How long a reboot, start, stop or delete may stay in flight before the
	// reconciler calls it failed. It covers a cold boot plus a WAL replay, which
	// is the longest a restart can honestly take.
	transitionTimeout = 10 * time.Minute
)

// The EC2 lifecycle states a DB VM may be in and still be on its way up. The
// reconciler will not call an instance available until its VM is running.
const instanceStateRunning = "running"

// The VM's EC2 lifecycle state, fanned out across every host so a DB VM is
// observed wherever it landed. Nil disables the VM half of the check.
type InstanceStateResolver interface {
	InstanceState(ctx context.Context, instanceID, accountID string) (string, error)
}

// The leader-elected RDS control loop. One node holds the lease and does the
// control work; every node keeps serving the API, so a leaderless gap delays a
// status transition without failing a request.
//
// Its responsibilities are the transitions no single API call can finish — the
// ones it drives itself and the ones whose caller died partway through — plus
// the failure classifier that gives a settled instance an honest health state.
// The backup sweep (rds-9) extends the same loop.
type Reconciler struct {
	svc    *Service
	holder string

	mu     sync.Mutex
	leader bool
}

// holder identifies this daemon in the lease.
func NewReconciler(svc *Service, holder string) *Reconciler {
	return &Reconciler{svc: svc, holder: holder}
}

// Drives the leadership and reconcile loop until ctx is cancelled. Intended as
// a daemon-boot goroutine; panics are the caller's recover concern.
func (r *Reconciler) Run(ctx context.Context) {
	leaseTicker := time.NewTicker(leaseRefresh)
	reconcileTicker := time.NewTicker(reconcileInterval)
	defer leaseTicker.Stop()
	defer reconcileTicker.Stop()

	r.evaluateLeadership(ctx)
	for {
		select {
		case <-ctx.Done():
			r.relinquish()
			return
		case <-leaseTicker.C:
			r.evaluateLeadership(ctx)
		case <-reconcileTicker.C:
			if !r.isLeader() {
				continue
			}
			if err := r.reconcileOnce(ctx); err != nil {
				slog.ErrorContext(ctx, "rds reconciler: pass failed", "holder", r.holder, "err", err)
			}
		}
	}
}

func (r *Reconciler) isLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leader
}

func (r *Reconciler) evaluateLeadership(ctx context.Context) {
	won := r.acquireOrRefresh(ctx)
	r.mu.Lock()
	was := r.leader
	r.leader = won
	r.mu.Unlock()

	switch {
	case won && !was:
		slog.Info("rds reconciler: elected leader", "holder", r.holder)
	case !won && was:
		slog.Info("rds reconciler: lost leadership", "holder", r.holder)
	}
}

// Claims the lease, or refreshes it (resetting the TTL) when we already hold it.
func (r *Reconciler) acquireOrRefresh(ctx context.Context) bool {
	kv, err := r.leaderBucket(ctx)
	if err != nil {
		return false
	}
	if _, err := kv.Create(ctx, reconcilerLeaderKey, []byte(r.holder)); err == nil {
		return true
	}
	entry, err := kv.Get(ctx, reconcilerLeaderKey)
	if err != nil {
		return false
	}
	if string(entry.Value()) != r.holder {
		return false
	}
	if _, err := kv.Put(ctx, reconcilerLeaderKey, []byte(r.holder)); err != nil {
		return false
	}
	return true
}

// Releases the lease on shutdown so the next leader is elected immediately
// rather than after the TTL.
func (r *Reconciler) relinquish() {
	// Run's ctx is already cancelled by the time this is called, so the release
	// runs on its own — a cancelled ctx would fail the delete.
	ctx := context.Background()
	kv, err := r.leaderBucket(ctx)
	if err != nil {
		return
	}
	if entry, gerr := kv.Get(ctx, reconcilerLeaderKey); gerr == nil && string(entry.Value()) == r.holder {
		if err := kv.Delete(ctx, reconcilerLeaderKey); err != nil {
			slog.Debug("rds reconciler: release lease failed", "holder", r.holder, "err", err)
		}
	}
}

func (r *Reconciler) leaderBucket(ctx context.Context) (jetstream.KeyValue, error) {
	js, err := r.svc.js()
	if err != nil {
		return nil, err
	}
	return InitLeaderBucket(ctx, js)
}

// One pass across every tenant. A bucket that cannot be read stops the pass
// with an error rather than being skipped silently, so a partial view shows up
// in the log instead of looking like a fleet with nothing to do.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	js, err := r.svc.js()
	if err != nil {
		return err
	}
	buckets, err := AccountBucketNames(ctx, js)
	if err != nil {
		return fmt.Errorf("rds reconciler: enumerate account buckets: %w", err)
	}
	var failures []error
	for _, bucket := range buckets {
		kv, err := js.KeyValue(ctx, bucket)
		if err != nil {
			failures = append(failures, fmt.Errorf("open %s: %w", bucket, err))
			continue
		}
		if err := r.reconcileAccount(ctx, kv, AccountIDFromBucketName(bucket)); err != nil {
			failures = append(failures, fmt.Errorf("reconcile %s: %w", bucket, err))
		}
	}
	return errors.Join(failures...)
}

func (r *Reconciler) reconcileAccount(ctx context.Context, kv jetstream.KeyValue, accountID string) error {
	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err := r.reconcileInstance(ctx, kv, accountID, id); err != nil {
			failures = append(failures, awserrors.Errorf(id, "%w", err))
		}
	}
	return errors.Join(failures...)
}

// The reconciler owns every transitional state that no single API call can
// finish: the one it drives itself (creating), and the ones whose caller may
// have died partway through. A settled instance is left alone.
func (r *Reconciler) reconcileInstance(ctx context.Context, kv jetstream.KeyValue, accountID, id string) error {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, DBInstanceKey(id), &rec)
	if err != nil || !found {
		return err
	}
	switch rec.Status {
	case StatusCreating:
		return r.reconcileCreating(ctx, kv, rev, accountID, &rec)
	case StatusRebooting, StatusStarting:
		return r.reconcileRestarting(ctx, kv, rev, accountID, &rec)
	case StatusModifying:
		return r.reconcileModifying(ctx, kv, rev, accountID, &rec)
	case StatusStopping:
		return r.reconcileStopping(ctx, kv, rev, accountID, &rec)
	case StatusDeleting:
		return r.reconcileDeleting(ctx, kv, rev, accountID, &rec)
	case StatusAvailable, StatusFailed:
		return r.reconcileHealth(ctx, kv, rev, accountID, &rec)
	default:
		return nil
	}
}

func (r *Reconciler) reconcileCreating(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	// No lower bound on the heartbeat: the VM is new, so any beat naming it is
	// necessarily this instance's.
	healthy, err := r.engineReady(ctx, accountID, rec, time.Time{})
	if err != nil {
		return err
	}
	if healthy {
		return r.transition(ctx, kv, rev, rec, StatusAvailable, "")
	}
	timeout := r.svc.bootstrapTimeout()
	if time.Since(rec.CreatedAt) > timeout {
		return r.transition(ctx, kv, rev, rec, StatusFailed,
			fmt.Sprintf("the database engine did not report healthy within %s of creation", timeout))
	}
	return nil
}

// Reboot and start both end the same way: the engine comes back and says so.
// The API call that began them returns before that happens, so this is what
// actually lands the instance in available.
func (r *Reconciler) reconcileRestarting(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	// The VM keeps its instance ID across a restart, so only a beat sent after
	// the transition began proves the engine came back rather than that it was
	// up before it went down.
	started := transitionStarted(rec)
	healthy, err := r.engineReady(ctx, accountID, rec, started)
	if err != nil {
		return err
	}
	if healthy {
		return r.transition(ctx, kv, rev, rec, StatusAvailable, "")
	}
	if time.Since(started) > transitionTimeout {
		return r.transition(ctx, kv, rev, rec, StatusFailed,
			fmt.Sprintf("the database engine did not report healthy within %s of %s", transitionTimeout, rec.Status))
	}
	return nil
}

// A modify is the one transition with work left on both sides of the VM coming
// back: the disruptive change itself, which a dead leader may have left
// half-applied, and the in-guest filesystem grow, which can only run once the
// agent is up again. Both are driven from here so a customer's modify completes
// without them, not just without them watching.
func (r *Reconciler) reconcileModifying(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	pending := rec.PendingModifiedValues
	overrun := time.Since(transitionStarted(rec)) > transitionTimeout

	// Still-unapplied disruptive values mean the modify never got as far as the
	// VM, so it is re-run rather than waited on: every step is idempotent, and
	// the record is what says which ones are outstanding.
	if !pending.empty() && !pending.growingFilesystem() {
		// The lease is what separates the two: a change still inside its own API
		// call holds it, and one whose worker died does not.
		resumed, err := r.svc.withModifyLease(ctx, kv, rec.DBInstanceIdentifier, func() error {
			return r.svc.applyPendingModifications(ctx, kv, accountID, rec)
		})
		switch {
		case err != nil && overrun:
			// Claiming and releasing the lease moved the record, so this pass's
			// revision is stale by now and a CAS on it would lose to our own
			// write on every pass — leaving the instance modifying forever.
			return r.transitionFresh(ctx, kv, rec.DBInstanceIdentifier, StatusFailed,
				fmt.Sprintf("the DB instance could not be modified within %s: %v", transitionTimeout, err))
		case err != nil:
			slog.WarnContext(ctx, "rds reconciler: resuming a modify failed; retrying next pass",
				"dbInstance", rec.DBInstanceIdentifier, "err", err)
		case !resumed && overrun:
			// Held for longer than the whole budget, so the holder is wedged
			// rather than working; failing it is what lets the customer retry.
			return r.transition(ctx, kv, rev, rec, StatusFailed,
				fmt.Sprintf("the DB instance was still being modified after %s", transitionTimeout))
		case !resumed:
			slog.DebugContext(ctx, "rds reconciler: a modify is already in flight; leaving it to its holder",
				"dbInstance", rec.DBInstanceIdentifier)
		}
		return nil
	}

	// The VM keeps its ID across a grow's restart and gets a new one across a
	// class change, so only a beat sent after the modify began proves the engine
	// is back rather than that it was up before the change started.
	healthy, err := r.engineReady(ctx, accountID, rec, transitionStarted(rec))
	if err != nil {
		return err
	}
	if !healthy {
		if overrun {
			return r.transition(ctx, kv, rev, rec, StatusFailed,
				fmt.Sprintf("the database engine did not report healthy within %s of the modification", transitionTimeout))
		}
		return nil
	}

	if pending.growingFilesystem() {
		if err := r.svc.finishFilesystemGrow(ctx, kv, accountID, rec); err != nil {
			if overrun {
				return r.transition(ctx, kv, rev, rec, StatusFailed,
					fmt.Sprintf("the filesystem could not be grown within %s: %v", transitionTimeout, err))
			}
			slog.WarnContext(ctx, "rds reconciler: extending the filesystem failed; retrying next pass",
				"dbInstance", rec.DBInstanceIdentifier, "err", err)
			return nil
		}
		// The record moved under the revision this pass read, so the transition
		// is left to the next one rather than raced.
		return nil
	}
	return r.transition(ctx, kv, rev, rec, StatusAvailable, "")
}

// A stop whose caller died leaves the VM possibly still running, so the stop is
// re-issued rather than assumed: it is idempotent, and a VM no node holds is
// confirmed down against the fleet before the record calls it stopped.
func (r *Reconciler) reconcileStopping(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	if r.svc.deps.Instances == nil {
		return errors.New("rds reconciler: no instance command path configured")
	}
	err := r.svc.deps.Instances.StopInstance(ctx, rec.InstanceID)
	if errors.Is(err, ErrInstanceNotOnNode) {
		err = r.svc.confirmVMNotRunning(ctx, accountID, rec.InstanceID)
	}
	if err == nil {
		return r.transition(ctx, kv, rev, rec, StatusStopped, "")
	}
	if time.Since(transitionStarted(rec)) > transitionTimeout {
		return r.transition(ctx, kv, rev, rec, StatusFailed,
			fmt.Sprintf("the DB instance could not be stopped within %s: %v", transitionTimeout, err))
	}
	slog.WarnContext(ctx, "rds reconciler: resuming a stop failed; retrying next pass",
		"dbInstance", rec.DBInstanceIdentifier, "err", err)
	return nil
}

// Re-runs the teardown from wherever it stopped. Every step tolerates a missing
// resource, so replaying it converges rather than failing on what it already
// did; only a teardown still stuck past the bound is called failed, which is
// what lets the customer retry the delete.
func (r *Reconciler) reconcileDeleting(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	err := r.svc.teardownDBInstance(ctx, kv, accountID, rec)
	if err == nil {
		return nil
	}
	if time.Since(transitionStarted(rec)) > transitionTimeout {
		return r.transition(ctx, kv, rev, rec, StatusFailed,
			fmt.Sprintf("the DB instance could not be deleted within %s: %v", transitionTimeout, err))
	}
	slog.WarnContext(ctx, "rds reconciler: resuming a delete failed; retrying next pass",
		"dbInstance", rec.DBInstanceIdentifier, "err", err)
	return nil
}

// When the transition began. A record written by an older control plane carries
// no stamp, so its last write stands in — it is never earlier than the
// transition, so the bound cannot be under-counted.
func transitionStarted(rec *DBInstanceRecord) time.Time {
	if rec.TransitionStartedAt != nil {
		return *rec.TransitionStartedAt
	}
	return rec.UpdatedAt
}

// Both halves must hold: a healthy heartbeat from the record's *current* VM,
// and that VM actually running. A stale beat from a superseded VM would
// otherwise report a replaced instance as ready. Beats at or before since are
// ignored, which is how a restart is told from the engine it restarted.
func (r *Reconciler) engineReady(ctx context.Context, accountID string, rec *DBInstanceRecord, since time.Time) (bool, error) {
	if rec.Agent.EngineHealth != EngineHealthHealthy || rec.InstanceID == "" ||
		rec.Agent.InstanceID != rec.InstanceID {
		return false, nil
	}
	if !r.heartbeatFresh(accountID, rec, since) {
		return false, nil
	}
	if r.svc.deps.InstanceState == nil {
		return true, nil
	}
	state, err := r.svc.deps.InstanceState.InstanceState(ctx, rec.InstanceID, accountID)
	if err != nil {
		return false, fmt.Errorf("resolve VM state for %s: %w", rec.InstanceID, err)
	}
	return state == instanceStateRunning, nil
}

// The in-memory beat is fresher than the persisted one but only this node sees
// it, so a leader that has seen no beat falls back to the record — which trails
// the truth by at most the persist floor.
func (r *Reconciler) heartbeatFresh(accountID string, rec *DBInstanceRecord, since time.Time) bool {
	lastSeen, ok := r.svc.LastSeen(accountID, rec.DBInstanceIdentifier)
	if !ok {
		if rec.Agent.LastSeen == nil {
			return false
		}
		lastSeen = *rec.Agent.LastSeen
	}
	if !since.IsZero() && !lastSeen.After(since) {
		return false
	}
	return time.Since(lastSeen) <= HeartbeatStaleAfter
}

// A CAS write, so a transition raced by an agent report or a lifecycle op is
// dropped rather than clobbering the newer state; the next pass re-reads.
func (r *Reconciler) transition(ctx context.Context, kv jetstream.KeyValue, rev uint64, rec *DBInstanceRecord, to Status, reason string) error {
	if !CanTransition(rec.Status, to) {
		return fmt.Errorf("illegal transition %s -> %s", rec.Status, to)
	}
	from := rec.Status
	rec.Status = to
	rec.FailureReason = reason
	rec.UpdatedAt = time.Now().UTC()

	if err := updateJSON(ctx, kv, DBInstanceKey(rec.DBInstanceIdentifier), rev, rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			slog.DebugContext(ctx, "rds reconciler: transition lost a revision race; retrying next pass",
				"dbInstance", rec.DBInstanceIdentifier, "to", to)
			return nil
		}
		return err
	}
	slog.InfoContext(ctx, "rds reconciler: DB instance transitioned",
		"dbInstance", rec.DBInstanceIdentifier, "from", from, "to", to, "reason", reason)
	return nil
}

// transition against a freshly read revision, for a caller that has written the
// record itself since the pass opened and so cannot use the revision it read.
func (r *Reconciler) transitionFresh(ctx context.Context, kv jetstream.KeyValue, id string, to Status, reason string) error {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, DBInstanceKey(id), &rec)
	if err != nil || !found {
		return err
	}
	return r.transition(ctx, kv, rev, &rec, to, reason)
}

// Adapts an EC2 describe fan-out to the reconciler's narrow state lookup.
type describeInstanceState struct {
	describe func(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
}

var _ InstanceStateResolver = (*describeInstanceState)(nil)

// The VM runs in the system account, so the describe is issued there; the
// customer account cannot see a platform-managed instance.
func NewDescribeInstanceState(describe func(*ec2.DescribeInstancesInput, string) (*ec2.DescribeInstancesOutput, error)) InstanceStateResolver {
	return &describeInstanceState{describe: describe}
}

func (d *describeInstanceState) InstanceState(_ context.Context, instanceID, _ string) (string, error) {
	out, err := d.describe(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if aws.StringValue(instance.InstanceId) == instanceID && instance.State != nil {
				return aws.StringValue(instance.State.Name), nil
			}
		}
	}
	// A VM the platform cannot find is not running, which is the answer the
	// caller needs — not an error that would stall the whole pass.
	return "", nil
}
