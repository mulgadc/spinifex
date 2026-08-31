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
	"github.com/mulgadc/spinifex/spinifex/kvlease"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// Lease and sweep timing. The holder refreshes well inside the bucket's 60s TTL,
// so a leader that dies is replaced within one TTL rather than one refresh.
const (
	reconcilerLeaderKey = "reconciler"
	leaseRefresh        = 20 * time.Second
	reconcileInterval   = 15 * time.Second

	// The drift backstop, and so also the cadence on which a backup or
	// maintenance window is noticed. A window is a half-hour slot, so noticing
	// one this late is well inside what it promises.
	reconcileResync = 5 * time.Minute

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

	lease    *kvlease.Lease
	leaseErr error

	// The payloads already reported as stuck pending. The condition persists
	// until an operator acts, so without this the event would be re-recorded
	// every sweep and crowd the bounded ring.
	reportedMu      sync.Mutex
	reportedPending map[string]string

	// The region's RDS system security group, resolved once. Ensuring it per
	// instance per pass would be a VPC and group describe for every DB VM every
	// 15s; the group is regional and nothing deletes it per instance.
	systemSGMu sync.Mutex
	systemSGID string
}

// holder identifies this daemon in the lease.
func NewReconciler(svc *Service, holder string) *Reconciler {
	r := &Reconciler{svc: svc, holder: holder, reportedPending: make(map[string]string)}
	r.lease, r.leaseErr = kvlease.New(kvlease.Config{
		Name:   "rds/reconciler",
		Bucket: r.leaderBucket,
		Key:    reconcilerLeaderKey,
		Holder: holder,
		TTL:    KVBucketRDSLeaderTTL,
		Renew:  leaseRefresh,
		Retry:  leaseRefresh,
	})
	return r
}

// Drives the leadership and reconcile loop until ctx is cancelled. Intended as
// a daemon-boot goroutine; panics are the caller's recover concern.
func (r *Reconciler) Run(ctx context.Context) {
	if r.leaseErr != nil {
		slog.ErrorContext(ctx, "rds reconciler: lease config invalid", "holder", r.holder, "err", r.leaseErr)
		return
	}
	leadershipCtx, cancelLeadership := context.WithCancel(ctx)
	leadershipDone := make(chan struct{})
	go func() {
		defer close(leadershipDone)
		r.lease.Run(leadershipCtx)
	}()
	defer func() {
		cancelLeadership()
		<-leadershipDone
	}()

	reconciler.Run(ctx, reconciler.Config{
		Name:      "rds",
		Sources:   []reconciler.Source{reconciler.Dynamic(r.watchBuckets, ">")},
		Reconcile: r.reconcilePass,
		Resync:    reconcileResync,
	})
}

// Every per-account bucket, re-read each resync so a new tenant is watched
// without a restart. The filter is the whole bucket because a DB instance key
// is one subject token, which no prefix wildcard can match.
func (r *Reconciler) watchBuckets(ctx context.Context) ([]*kvstore.Bucket, error) {
	js, err := r.svc.js()
	if err != nil {
		return nil, err
	}
	return AccountWatchBuckets(ctx, js)
}

// One pass, and when the loop should run again with nothing having changed.
func (r *Reconciler) reconcilePass(ctx context.Context) (time.Duration, error) {
	if !r.isLeader() {
		// Leadership changes without anything being written, so a follower has
		// to keep asking. The pass itself is a lease check and nothing more.
		return leaseRefresh, nil
	}
	revisit, err := r.reconcileOnce(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "rds reconciler: pass failed", "holder", r.holder, "err", err)
	}
	// Already logged, so it is not returned again: the loop would log it a
	// second time under its own message.
	return revisit, nil
}

// The shared GC backstop's cluster-wide gate. The reconciler's lease is already
// cluster-singular and held continuously rather than claimed per sweep, so
// holding it is the whole answer and there is nothing for the caller to release.
func (r *Reconciler) AcquireClusterLease() (func(), bool) {
	return func() {}, r.isLeader()
}

func (r *Reconciler) isLeader() bool {
	return r.lease.Held()
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
func (r *Reconciler) reconcileOnce(ctx context.Context) (time.Duration, error) {
	js, err := r.svc.js()
	if err != nil {
		return 0, err
	}
	buckets, err := AccountBucketNames(ctx, js)
	if err != nil {
		return 0, fmt.Errorf("rds reconciler: enumerate account buckets: %w", err)
	}
	var revisit time.Duration
	var failures []error
	for _, bucket := range buckets {
		kv, err := js.KeyValue(ctx, bucket)
		if err != nil {
			failures = append(failures, fmt.Errorf("open %s: %w", bucket, err))
			continue
		}
		due, err := r.reconcileAccount(ctx, kv, AccountIDFromBucketName(bucket))
		revisit = reconciler.Earliest(revisit, due)
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile %s: %w", bucket, err))
		}
	}
	return revisit, errors.Join(failures...)
}

func (r *Reconciler) reconcileAccount(ctx context.Context, kv jetstream.KeyValue, accountID string) (time.Duration, error) {
	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		return 0, err
	}
	var revisit time.Duration
	var failures []error
	for _, id := range ids {
		due, err := r.reconcileInstance(ctx, kv, accountID, id)
		revisit = reconciler.Earliest(revisit, due)
		if err != nil {
			failures = append(failures, awserrors.Errorf(id, "%w", err))
		}
		if err := r.reconcileWindows(ctx, kv, accountID, id); err != nil {
			failures = append(failures, awserrors.Errorf(id, "%w", err))
		}
	}
	due, err := r.reconcileSnapshots(ctx, kv, accountID)
	revisit = reconciler.Earliest(revisit, due)
	if err != nil {
		failures = append(failures, err)
	}
	return revisit, errors.Join(failures...)
}

// The reconciler owns every transitional state that no single API call can
// finish: the one it drives itself (creating), and the ones whose caller may
// have died partway through. A settled instance is left alone.
func (r *Reconciler) reconcileInstance(ctx context.Context, kv jetstream.KeyValue, accountID, id string) (time.Duration, error) {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, DBInstanceKey(id), &rec)
	if err != nil || !found {
		return 0, err
	}
	// Read from the record as it stands, not as a handler below leaves it: a
	// handler that moves the status writes to KV, and that write wakes the loop
	// on its own.
	revisit := r.revisitFor(accountID, &rec)
	if err := r.reportStalePendingBootstrap(ctx, kv, accountID, &rec); err != nil {
		return revisit, err
	}
	remediated, err := r.remediateSystemENISG(ctx, kv, rev, &rec)
	if err != nil || remediated {
		return revisit, err
	}
	var handled error
	switch rec.Status {
	case StatusCreating:
		handled = r.reconcileCreating(ctx, kv, rev, accountID, &rec)
	case StatusRebooting, StatusStarting:
		handled = r.reconcileRestarting(ctx, kv, rev, accountID, &rec)
	case StatusModifying:
		handled = r.reconcileModifying(ctx, kv, rev, accountID, &rec)
	case StatusStopping:
		handled = r.reconcileStopping(ctx, kv, rev, accountID, &rec)
	case StatusDeleting:
		handled = r.reconcileDeleting(ctx, kv, rev, accountID, &rec)
	case StatusBackingUp:
		handled = r.reconcileBackingUp(ctx, kv, rev, accountID, &rec)
	case StatusAvailable, StatusFailed:
		handled = r.reconcileHealth(ctx, kv, rev, accountID, &rec)
	}
	return revisit, handled
}

// When this instance next needs looking at with nothing having changed, which
// is the only thing a watch cannot tell the loop.
//
// A transitional record is waiting on a VM lifecycle state that lives in EC2 and
// is never written to KV, so there is no signal to wait for and it keeps the
// interval the ticker used to provide. A settled one is waiting for its
// heartbeat to go stale — silence, which by definition writes nothing — so it
// asks to be looked at the instant the beat expires, which is sharper than the
// ticker managed. Anything else is genuinely inert until someone writes.
func (r *Reconciler) revisitFor(accountID string, rec *DBInstanceRecord) time.Duration {
	switch rec.Status {
	case StatusCreating, StatusRebooting, StatusStarting, StatusModifying,
		StatusStopping, StatusDeleting, StatusBackingUp:
		return reconcileInterval
	case StatusFailed:
		// Terminal for the control plane, and recovery arrives as a heartbeat,
		// which is a write the watch already sees.
		return 0
	case StatusAvailable:
		return r.settledRevisit(accountID, rec)
	default:
		return 0
	}
}

// The deadline for an available instance, which is whichever of its two clocks
// runs out first.
//
// Before the beat expires, that is the expiry itself. After it, the failure
// clock is what decides: the instance is dark but stays available until the
// grace window passes and a later pass agrees it is still dark. Returning
// nothing here was wrong — it left the pass that does the failing with nothing
// to schedule it, so a dead database waited for the resync.
func (r *Reconciler) settledRevisit(accountID string, rec *DBInstanceRecord) time.Duration {
	lastSeen, bound := r.lastHeartbeat(accountID, rec)
	if lastSeen.IsZero() {
		// Never reported at all, so there is no beat to expire and the failure
		// clock is the only one running.
		return r.failureRevisit(rec)
	}
	if expiry := time.Until(lastSeen.Add(bound)); expiry > 0 {
		return expiry
	}
	return r.failureRevisit(rec)
}

// How long until the failure grace window closes. An unstamped clock is stamped
// by the pass that just ran, so the window starts from here.
func (r *Reconciler) failureRevisit(rec *DBInstanceRecord) time.Duration {
	grace := r.svc.failureGrace()
	if rec.UnhealthySince == nil {
		return grace
	}
	if remaining := time.Until(rec.UnhealthySince.Add(grace)); remaining > 0 {
		return remaining
	}
	// Already due and still not failed, which is the degraded reading: dark agent,
	// live VM. Nothing has changed, so retry at the old interval rather than
	// immediately, which would spin for as long as the condition holds.
	return reconcileInterval
}

// Moves a system NIC launched before the RDS system security group existed onto
// it, once. Reports whether it acted, so the caller yields this pass and the
// status handler runs on the next tick against a fresh revision.
//
// vpcd applies the requested group list declaratively, so this removes the ENI
// from the system VPC's default group rather than merely adding the new one
// alongside it — and that default group is the whole of the exposure.
func (r *Reconciler) remediateSystemENISG(ctx context.Context, kv jetstream.KeyValue,
	rev uint64, rec *DBInstanceRecord) (bool, error) {
	if rec.SystemENIID == "" || rec.Status == StatusDeleting {
		return false, nil
	}
	sgID, err := r.ensuredSystemSG(ctx)
	if err != nil {
		return false, err
	}
	if rec.SystemSGID == sgID {
		return false, nil
	}

	if _, err := r.svc.deps.Launch.VPC.ModifyNetworkInterfaceAttribute(ctx, &ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(rec.SystemENIID),
		Groups:             aws.StringSlice([]string{sgID}),
	}, utils.GlobalAccountID); err != nil {
		return false, fmt.Errorf("rds: move the system NIC of %s onto %s: %w", rec.DBInstanceIdentifier, sgID, err)
	}
	// Recorded only once vpcd has accepted the change, so a failure above is
	// retried next pass rather than remembered as done.
	rec.SystemSGID = sgID
	if _, err := updateJSONRevision(ctx, kv, DBInstanceKey(rec.DBInstanceIdentifier), rev, rec); err != nil {
		return false, err
	}
	slog.InfoContext(ctx, "rds: system NIC moved onto the RDS system security group",
		"dbInstance", rec.DBInstanceIdentifier, "eniId", rec.SystemENIID, "groupId", sgID)
	return true, nil
}

// The region's system security group, ensured on first use and then cached for
// the life of the process.
func (r *Reconciler) ensuredSystemSG(ctx context.Context) (string, error) {
	r.systemSGMu.Lock()
	defer r.systemSGMu.Unlock()
	if r.systemSGID != "" {
		return r.systemSGID, nil
	}
	deps := r.svc.deps.Launch
	if deps.VPC == nil || deps.Config == nil {
		return "", errors.New("rds reconciler: no VPC path is configured to place system NICs on their own security group")
	}
	refs, err := EnsureSystemVPC(ctx, deps.SystemVPC, &deps.Config.RDS, utils.GlobalAccountID, deps.Config.Region)
	if err != nil {
		return "", err
	}
	sgID, err := EnsureSystemSecurityGroup(ctx, deps.VPC, utils.GlobalAccountID, deps.Config.Region, refs.VpcID)
	if err != nil {
		return "", err
	}
	r.systemSGID = sgID
	return sgID, nil
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
func (r *Reconciler) reconcileSnapshots(ctx context.Context, kv jetstream.KeyValue, accountID string) (time.Duration, error) {
	ids, err := ListDBSnapshotIDs(ctx, kv)
	if err != nil {
		return 0, fmt.Errorf("list DB snapshots: %w", err)
	}
	var revisit time.Duration
	var failures []error
	for _, id := range ids {
		var rec DBSnapshotRecord
		rev, found, err := getJSONRevision(ctx, kv, DBSnapshotKey(id), &rec)
		if err != nil {
			failures = append(failures, fmt.Errorf("read DB snapshot %s: %w", id, err))
			continue
		}
		if !found || rec.Status != SnapshotStatusCreating {
			continue
		}
		// Not due yet, so ask to be back when it is rather than leaving a dead
		// worker's record to the resync.
		if due := time.Until(rec.CreatedAt.Add(snapshotResolveTimeout)); due > 0 {
			revisit = reconciler.Earliest(revisit, due)
			continue
		}
		if err := r.resolveCreatingSnapshot(ctx, kv, rev, accountID, &rec); err != nil {
			failures = append(failures, fmt.Errorf("resolve DB snapshot %s: %w", id, err))
		}
	}
	return revisit, errors.Join(failures...)
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
			case errors.Is(err, jetstream.ErrKeyNotFound), errors.Is(err, jetstream.ErrKeyRevisionMismatch):
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
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
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
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
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
