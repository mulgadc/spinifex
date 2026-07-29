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
// This phase's responsibilities are the creating → available transition and
// marking a stalled bootstrap failed. Auto-recovery (rds-6) and the backup
// sweep (rds-9) extend the same loop.
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
			failures = append(failures, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

// Only creating instances are acted on in this phase. Everything else is a
// later phase's transition, and touching it here would race the owner.
func (r *Reconciler) reconcileInstance(ctx context.Context, kv jetstream.KeyValue, accountID, id string) error {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, DBInstanceKey(id), &rec)
	if err != nil || !found {
		return err
	}
	if rec.Status != StatusCreating {
		return nil
	}

	healthy, err := r.bootstrapComplete(ctx, accountID, &rec)
	if err != nil {
		return err
	}
	if healthy {
		return r.transition(ctx, kv, rev, &rec, StatusAvailable, "")
	}
	timeout := r.svc.bootstrapTimeout()
	if time.Since(rec.CreatedAt) > timeout {
		return r.transition(ctx, kv, rev, &rec, StatusFailed,
			fmt.Sprintf("the database engine did not report healthy within %s of creation", timeout))
	}
	return nil
}

// Both halves must hold: a healthy heartbeat from the record's *current* VM,
// and that VM actually running. A stale beat from a superseded VM would
// otherwise report a replaced instance as ready.
func (r *Reconciler) bootstrapComplete(ctx context.Context, accountID string, rec *DBInstanceRecord) (bool, error) {
	if rec.Agent.EngineHealth != EngineHealthHealthy || rec.InstanceID == "" ||
		rec.Agent.InstanceID != rec.InstanceID {
		return false, nil
	}
	if !r.heartbeatFresh(accountID, rec) {
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
func (r *Reconciler) heartbeatFresh(accountID string, rec *DBInstanceRecord) bool {
	lastSeen, ok := r.svc.LastSeen(accountID, rec.DBInstanceIdentifier)
	if !ok {
		if rec.Agent.LastSeen == nil {
			return false
		}
		lastSeen = *rec.Agent.LastSeen
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
