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

	// How long a stop waits for the fleet to report the VM down, unless the
	// config overrides it. A node accepts a stop command milliseconds after it is
	// sent but takes seconds to drain and detach the data volume, and it is the
	// detach a storage grow is actually waiting on.
	defaultVMStopTimeout = 60 * time.Second

	// How long a DB snapshot record may sit in creating before the reconciler
	// settles it against EC2. Comfortably past the snapshot request's own budget,
	// so a record still being written by a live worker is never touched.
	snapshotResolveTimeout = 15 * time.Minute
)

// The EC2 lifecycle states a DB VM may be in and still be on its way up. The
// reconciler will not call an instance available until its VM is running.
const instanceStateRunning = "running"

// The states in which the VM is down and has released its data volume. A VM
// that is merely stopping or shutting-down still has it mapped, which is the
// reading a just-issued stop returns and the one ModifyVolume refuses.
const (
	instanceStateStopped    = "stopped"
	instanceStateTerminated = "terminated"
)

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
// The backup sweep extends the same loop.
type Reconciler struct {
	svc    *Service
	holder string

	mu     sync.Mutex
	leader bool

	// The payloads already reported as stuck pending. The condition persists
	// until an operator acts, so without this the event would be re-recorded
	// every sweep and crowd the bounded ring.
	reportedMu      sync.Mutex
	reportedPending map[string]string
}

// holder identifies this daemon in the lease.
func NewReconciler(svc *Service, holder string) *Reconciler {
	return &Reconciler{svc: svc, holder: holder, reportedPending: make(map[string]string)}
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

// The shared GC backstop's cluster-wide gate. The reconciler's lease is already
// cluster-singular and held continuously rather than claimed per sweep, so
// holding it is the whole answer and there is nothing for the caller to release.
func (r *Reconciler) AcquireClusterLease() (func(), bool) {
	return func() {}, r.isLeader()
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
		if err := r.reconcileWindows(ctx, kv, accountID, id); err != nil {
			failures = append(failures, awserrors.Errorf(id, "%w", err))
		}
	}
	if err := r.reconcileSnapshots(ctx, kv, accountID); err != nil {
		failures = append(failures, err)
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
	if err := r.reportStalePendingBootstrap(ctx, kv, accountID, &rec); err != nil {
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
	case StatusBackingUp:
		return r.reconcileBackingUp(ctx, kv, rev, accountID, &rec)
	case StatusAvailable, StatusFailed:
		return r.reconcileHealth(ctx, kv, rev, accountID, &rec)
	default:
		return nil
	}
}

// An available instance whose payload is still staged bootstrapped against an
// agent too old to acknowledge, so the ciphertext will sit there until an
// operator acts. Only reported: deleting the payload on the grounds that a
// healthy engine implies a completed bootstrap is exactly the inference this
// protocol refuses to make.
func (r *Reconciler) reportStalePendingBootstrap(ctx context.Context, kv jetstream.KeyValue,
	accountID string, rec *DBInstanceRecord) error {
	key := accountID + "/" + rec.DBInstanceIdentifier
	stale := rec.Status == StatusAvailable && time.Since(rec.CreatedAt) > r.svc.bootstrapTimeout()
	if !stale {
		r.forgetPendingReport(key)
		return nil
	}
	envelope, _, err := readBootstrapPayload(ctx, kv, rec.DBInstanceIdentifier)
	if err != nil {
		return err
	}
	if envelope == nil {
		r.forgetPendingReport(key)
		return nil
	}
	if !r.notePendingReport(key, envelope.PayloadID) {
		return nil
	}
	slog.WarnContext(ctx, "rds: a staged bootstrap payload is still pending on an available DB instance",
		"dbInstance", rec.DBInstanceIdentifier, "accountId", accountID, "payloadId", envelope.PayloadID)
	r.svc.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"The database engine is serving but has not confirmed its initial master credentials were applied; the staged credentials remain stored until it does.",
		EventCategoryNotification)
	return nil
}

// Reports whether this payload is newly stuck, so the event is recorded once per
// payload for as long as this node holds the lease.
func (r *Reconciler) notePendingReport(key, payloadID string) bool {
	r.reportedMu.Lock()
	defer r.reportedMu.Unlock()
	if r.reportedPending[key] == payloadID {
		return false
	}
	r.reportedPending[key] = payloadID
	return true
}

func (r *Reconciler) forgetPendingReport(key string) {
	r.reportedMu.Lock()
	defer r.reportedMu.Unlock()
	delete(r.reportedPending, key)
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
		reason := fmt.Sprintf("the database engine did not report healthy within %s of creation", timeout)
		if rec.Agent.Message != "" {
			reason += ": " + rec.Agent.Message
		}
		return r.transition(ctx, kv, rev, rec, StatusFailed, reason)
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
		resumed, err := r.svc.withModifyLease(ctx, kv, rec.DBInstanceIdentifier, func(applyCtx context.Context) error {
			return r.svc.applyPendingModifications(applyCtx, kv, accountID, rec)
		})
		switch {
		case errors.Is(err, errModifyLeaseLost):
			// Ownership moved or expired; leave the pending transition for its
			// current holder or the next pass rather than failing it underneath them.
			return nil
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
		err = r.svc.confirmVMStopped(ctx, accountID, rec.InstanceID)
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
	err := r.svc.teardownDBInstance(ctx, kv, accountID, rec, false)
	if err == nil || errors.Is(err, errFinalSnapshotInProgress) {
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

// A snapshot holds its instance in backing-up for the length of the snapshot and
// no longer, so an instance still there past the bound belongs to a worker that
// died. It is returned to where the snapshot found it; the quiesce needs no
// undoing, because the agent's own hold deadline is far shorter than this bound
// and has already released the engine.
func (r *Reconciler) reconcileBackingUp(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	if time.Since(transitionStarted(rec)) <= transitionTimeout {
		return nil
	}
	// The operation and the backing-up status are written in one CAS, so a record
	// without one predates this phase; the health classifier settles the guess.
	resume := StatusAvailable
	if rec.SnapshotOperation != nil {
		resume = rec.SnapshotOperation.ResumeStatus
	}
	rec.SnapshotOperation = nil
	slog.WarnContext(ctx, "rds reconciler: a snapshot left its instance in backing-up; returning it",
		"dbInstance", rec.DBInstanceIdentifier, "accountId", accountID, "resume", resume)
	return r.transition(ctx, kv, rev, rec, resume, "")
}

// The two scheduled passes: the automated backup's backup window is
// due, and the deferred modify its maintenance window opens. Both ride the leader
// lease, and both are driven from a persisted stamp rather than a timer, so a
// leader change cannot fire either of them twice for one window.
//
// The record is re-read rather than carried over from the status pass above,
// which may have transitioned the instance — a backup must not be started against
// a status that has since moved.
func (r *Reconciler) reconcileWindows(ctx context.Context, kv jetstream.KeyValue, accountID, id string) error {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, DBInstanceKey(id), &rec)
	if err != nil || !found {
		return err
	}
	// A backup holds the instance in backing-up and a maintenance apply moves it to
	// modifying, so the two can never run together: whichever fires owns the pass.
	fired, err := r.svc.runBackupWindow(ctx, kv, rev, accountID, &rec)
	if fired || err != nil {
		return err
	}
	_, err = r.svc.runMaintenanceWindow(ctx, kv, rev, accountID, &rec)
	return err
}

// Settles the DB snapshot records a create never finished writing. The EC2
// snapshot is the authority: it was tagged with the DB snapshot identifier before
// the record was flipped, so its presence says the data exists and its absence
// says the create died before cutting it.
func (r *Reconciler) reconcileSnapshots(ctx context.Context, kv jetstream.KeyValue, accountID string) error {
	ids, err := ListDBSnapshotIDs(ctx, kv)
	if err != nil {
		return fmt.Errorf("list DB snapshots: %w", err)
	}
	var failures []error
	for _, id := range ids {
		var rec DBSnapshotRecord
		rev, found, err := getJSONRevision(ctx, kv, DBSnapshotKey(id), &rec)
		if err != nil {
			failures = append(failures, fmt.Errorf("read DB snapshot %s: %w", id, err))
			continue
		}
		if !found || rec.Status != SnapshotStatusCreating ||
			time.Since(rec.CreatedAt) <= snapshotResolveTimeout {
			continue
		}
		if err := r.resolveCreatingSnapshot(ctx, kv, rev, accountID, &rec); err != nil {
			failures = append(failures, fmt.Errorf("resolve DB snapshot %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

// Either adopts the EC2 snapshot the dead worker cut, or withdraws the record so
// its identifier is usable again. A record left in creating would otherwise hold
// the name forever while naming nothing a customer can restore.
func (r *Reconciler) resolveCreatingSnapshot(ctx context.Context, kv jetstream.KeyValue, rev uint64,
	accountID string, rec *DBSnapshotRecord) error {
	snapshotID, err := r.svc.findEC2SnapshotFor(ctx, accountID, rec.DBSnapshotIdentifier)
	if err != nil {
		return err
	}
	if snapshotID == "" {
		if err := kv.Delete(ctx, DBSnapshotKey(rec.DBSnapshotIdentifier), jetstream.LastRevision(rev)); err != nil {
			switch {
			case errors.Is(err, jetstream.ErrKeyNotFound), errors.Is(err, jetstream.ErrKeyExists):
				// A concurrent completion or delete owns the newer revision.
				return nil
			default:
				return err
			}
		}
		r.svc.RecordEvent(ctx, accountID, EventSourceTypeDBSnapshot, rec.DBSnapshotIdentifier,
			"The DB snapshot could not be completed and has been removed.",
			EventCategoryFailure, EventCategoryBackup)
		slog.WarnContext(ctx, "rds reconciler: withdrew a DB snapshot whose data was never cut",
			"dbSnapshot", rec.DBSnapshotIdentifier, "accountId", accountID)
		return nil
	}

	rec.SnapshotID = snapshotID
	rec.Status = SnapshotStatusAvailable
	// The worker died before it could report whether the quiesce held, so the
	// conservative reading is the one that never overstates the snapshot.
	rec.CrashConsistent = true
	if err := updateJSON(ctx, kv, DBSnapshotKey(rec.DBSnapshotIdentifier), rev, rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil
		}
		return err
	}
	r.svc.RecordEvent(ctx, accountID, EventSourceTypeDBSnapshot, rec.DBSnapshotIdentifier,
		"DB snapshot created.", EventCategoryBackup, EventCategoryCreation)
	slog.InfoContext(ctx, "rds reconciler: adopted the EC2 snapshot of an unfinished DB snapshot",
		"dbSnapshot", rec.DBSnapshotIdentifier, "snapshotId", snapshotID, "accountId", accountID)
	return nil
}

// The EC2 snapshot an account-scoped DB snapshot identifier owns, empty when
// none was cut. Strict lookup is required because a partial metadata scan cannot
// prove absence and must never cause reconciliation to withdraw the RDS record.
func (s *Service) findEC2SnapshotFor(ctx context.Context, accountID, dbSnapshotIdentifier string) (string, error) {
	if s.deps.Snapshots == nil {
		return "", errors.New("rds: no snapshot service configured")
	}
	out, err := s.deps.Snapshots.DescribeSnapshotsStrict(ctx, &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:" + rdsSnapshotTagKey),
				Values: aws.StringSlice([]string{dbSnapshotIdentifier}),
			},
			{
				Name:   aws.String("tag:" + rdsSnapshotAccountTagKey),
				Values: aws.StringSlice([]string{accountID}),
			},
		},
	}, utils.GlobalAccountID)
	if err != nil {
		if awserrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("rds: find the EC2 snapshot of %s in account %s: %w",
			dbSnapshotIdentifier, accountID, err)
	}
	if out == nil {
		return "", nil
	}

	var found string
	for _, snapshot := range out.Snapshots {
		id := aws.StringValue(snapshot.SnapshotId)
		if id == "" {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("rds: DB snapshot %s in account %s has multiple EC2 snapshots (%s and %s)",
				dbSnapshotIdentifier, accountID, found, id)
		}
		found = id
	}
	return found, nil
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

// Both halves must hold: a fresh healthy heartbeat from the record's *current*
// VM, and that VM actually running. A stale beat from a superseded VM would
// otherwise report a replaced instance as ready. The observation is the same one
// the health classifier forms, so freshness is judged by one rule package-wide.
func (r *Reconciler) engineReady(ctx context.Context, accountID string, rec *DBInstanceRecord, since time.Time) (bool, error) {
	obs := r.observeAgent(accountID, rec)
	if !obs.engineHealthy || !obs.heartbeatFresh {
		return false, nil
	}
	// A beat at or before since came from the engine the restart replaced, so it
	// says nothing about the one that took its place.
	if !since.IsZero() && !obs.lastSeen.After(since) {
		return false, nil
	}
	return r.vmRunning(ctx, accountID, rec)
}

// A CAS write, so a transition raced by an agent report or a lifecycle op is
// dropped rather than clobbering the newer state; the next pass re-reads.
func (r *Reconciler) transition(ctx context.Context, kv jetstream.KeyValue, rev uint64, rec *DBInstanceRecord, to Status, reason string) error {
	if !CanTransition(rec.Status, to) {
		return fmt.Errorf("illegal transition %s -> %s", rec.Status, to)
	}
	from := rec.Status
	rec.Status = to
	if from == StatusCreating {
		// A healthy engine proves the initial format completed; a failed create
		// is no longer an active initialization either. Never retain a reusable
		// grant after leaving the create operation.
		rec.FormatAuthorized = false
	}
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
	if from == StatusCreating && to == StatusAvailable {
		r.svc.RecordEvent(ctx, AccountIDFromBucketName(kv.Bucket()), EventSourceTypeDBInstance,
			rec.DBInstanceIdentifier, "DB instance is available.", EventCategoryAvailability)
	}
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
