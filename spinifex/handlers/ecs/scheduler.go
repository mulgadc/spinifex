package handlers_ecs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/mulgadc/spinifex/spinifex/kvlease"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Scheduler timing. The leader lease lives in spinifex-ecs-leader (60s TTL); the
// holder refreshes well inside that. The reaper marks an instance DRAINING after
// three missed 30s heartbeats.
const (
	schedulerLeaderKey  = "scheduler"
	leaseRefresh        = 20 * time.Second
	reaperInterval      = 30 * time.Second
	heartbeatTimeout    = 90 * time.Second
	stoppedReasonReaped = "ContainerInstance disconnected"

	// reconcileInterval is how soon a service with something in flight is
	// revisited. A settled service asks for nothing and is woken by the bus.
	reconcileInterval = 10 * time.Second

	// sweepInterval is what a failed sweep retries at, having learnt no deadline;
	// stoppedTaskRetention keeps a just-stopped task describable (DescribeTasks /
	// UI exit reason) before it is dropped, matching AWS's ~1h STOPPED window.
	sweepInterval        = 60 * time.Second
	stoppedTaskRetention = 1 * time.Hour

	// convergeInterval is how often the leader re-asserts the instance-role
	// policy. The document changes only across releases, so this trades a slower
	// pickup after an upgrade for a fleet-wide write every reconcile tick.
	convergeInterval = 5 * time.Minute

	// reconcileResync is the drift backstop for both scheduler loops, and the
	// outer bound on every deadline they return.
	reconcileResync = 5 * time.Minute
)

// Scheduler is the per-daemon ECS control loop. A single leader (elected via the
// shared leader bucket) owns the Layer-2 bus subscriptions and the heartbeat
// reaper; losers idle and retry. Placement itself happens synchronously in the
// RunTask handler, so a brief leaderless gap never blocks RunTask.
type Scheduler struct {
	nc     *nats.Conn
	svc    *Service
	holder string

	lease    *kvlease.Lease
	leaseErr error

	// wake carries a bus message that changed something the service reconcile
	// reads. Fed by the task-state subscription and not by the heartbeat one:
	// that separation is the whole reason this loop needs no KV watch.
	wake chan struct{}

	// passMu keeps the two loops from running a pass at the same time. They are
	// separate schedules over the same records, and the four tickers they replace
	// shared one goroutine, so the leader stays the single writer it was.
	passMu sync.Mutex

	// lastConverge stamps the instance-role converge, which rides the timer pass
	// rather than a ticker. Guarded by passMu, like the pass that writes it.
	lastConverge time.Time

	mu   sync.Mutex
	subs []*nats.Subscription
}

// NewScheduler constructs a Scheduler. holder identifies this daemon in the lease.
func NewScheduler(nc *nats.Conn, svc *Service, holder string) *Scheduler {
	// One slot: it means "something the reconcile reads has changed", and a
	// second arriving before the pass runs says nothing the first did not.
	sc := &Scheduler{nc: nc, svc: svc, holder: holder, wake: make(chan struct{}, 1)}
	sc.lease, sc.leaseErr = kvlease.New(kvlease.Config{
		Name:   "ecs/scheduler",
		Bucket: sc.leaderBucket,
		Key:    schedulerLeaderKey,
		Holder: holder,
		TTL:    KVBucketECSLeaderTTL,
		Renew:  leaseRefresh,
		Retry:  leaseRefresh,
		// The leader owns the Layer-2 bus subscriptions and the instance-role
		// converge pass. A node that can do neither stands down rather than hold
		// the lease and process nothing.
		OnGained: sc.onGained,
		OnLost:   sc.unsubscribeBus,
	})
	return sc
}

func (sc *Scheduler) leaderBucket(ctx context.Context) (jetstream.KeyValue, error) {
	js, err := jetstream.New(sc.nc)
	if err != nil {
		return nil, err
	}
	return InitLeaderBucket(ctx, js)
}

// Run drives the leadership + reaper loop until ctx is cancelled. It is intended
// to run as a daemon-boot goroutine; panics are the caller's recover concern.
func (sc *Scheduler) Run(ctx context.Context) {
	if sc.leaseErr != nil {
		slog.ErrorContext(ctx, "ECS scheduler: lease config invalid", "holder", sc.holder, "err", sc.leaseErr)
		return
	}
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		sc.lease.Run(ctx)
	}()

	// Two loops rather than four tickers, split by what wakes them. Services are
	// woken by the bus and nothing else; the reaper, the sweep and the converge
	// are woken by elapsed time and nothing else. Neither watches KV: the account
	// bucket carries a heartbeat write per instance per interval, which no filter
	// can exclude, so watching it would scale wake-ups with the fleet.
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		reconciler.Run(ctx, reconciler.Config{
			Name:      "ecs/services",
			Reconcile: sc.reconcileServicesPass,
			Trigger:   sc.wake,
			Resync:    reconcileResync,
		})
	}()
	go func() {
		defer loops.Done()
		reconciler.Run(ctx, reconciler.Config{
			Name:      "ecs/timers",
			Reconcile: sc.timerPass,
			Resync:    reconcileResync,
		})
	}()

	<-ctx.Done()
	loops.Wait()
	// The daemon waits on Run, so the lease delete must complete before it
	// returns, otherwise the next leader waits out the full TTL.
	<-leaseDone
}

// reconcileServicesPass converges every service and says when to come back. A
// settled fleet asks for no deadline at all: what would change it is a task
// state report, and that arrives as a wake.
func (sc *Scheduler) reconcileServicesPass(ctx context.Context) (time.Duration, error) {
	if !sc.lease.Held() {
		// Leadership turns over without anything being published, so a follower
		// has to keep asking. The pass itself is a lease check and nothing more.
		return leaseRefresh, nil
	}
	sc.passMu.Lock()
	defer sc.passMu.Unlock()
	revisit, err := sc.svc.reconcileAllServices(ctx)
	if err != nil {
		slog.Error("ECS scheduler: pass failed", "pass", "service reconcile", "err", err)
		return reconcileInterval, nil
	}
	return revisit, nil
}

// timerPass runs the three jobs that only elapsed time can trigger, and returns
// the soonest of their deadlines. None of them is announced by a write, so
// without this they would run on the resync alone.
func (sc *Scheduler) timerPass(ctx context.Context) (time.Duration, error) {
	if !sc.lease.Held() {
		return leaseRefresh, nil
	}
	sc.passMu.Lock()
	defer sc.passMu.Unlock()
	reapDue, err := sc.reap(ctx)
	if err != nil {
		slog.Error("ECS scheduler: pass failed", "pass", "instance reap", "err", err)
		reapDue = reaperInterval
	}
	sweepDue, err := sc.sweepStoppedTasks(ctx)
	if err != nil {
		slog.Error("ECS scheduler: pass failed", "pass", "stopped-task sweep", "err", err)
		sweepDue = sweepInterval
	}
	convergeDue := sc.convergeIfDue(ctx)
	return reconciler.Earliest(reconciler.Earliest(reapDue, sweepDue), convergeDue), nil
}

// convergeIfDue re-asserts the instance-role policy when its interval has
// elapsed, and reports how long until it is due again. It rides this pass rather
// than a ticker of its own because the document changes only across releases, so
// the exact cadence does not matter.
func (sc *Scheduler) convergeIfDue(ctx context.Context) time.Duration {
	if remaining := time.Until(sc.lastConverge.Add(convergeInterval)); remaining > 0 {
		return remaining
	}
	sc.lastConverge = time.Now()
	if err := sc.convergeInstanceRoles(ctx); err != nil {
		slog.Error("ECS scheduler: pass failed", "pass", "instance role converge", "err", err)
	}
	return convergeInterval
}

// onGained refuses the lease when this node cannot converge instance roles.
// A leader without IAM holds the lease and never writes the grant, so every
// agent's credentials lapse an hour later with nothing but a per-tick log.
func (sc *Scheduler) onGained(ctx context.Context) error {
	if sc.svc.deps.IAM == nil {
		return errors.New("IAM dependency not wired: cannot converge instance roles")
	}
	return sc.subscribeBus(ctx)
}

// subscribeBus wires the wildcard Layer-2 subscriptions onto the service KV
// writers. All clusters/accounts fan into the leader.
func (sc *Scheduler) subscribeBus(_ context.Context) error {
	register, err := sc.nc.Subscribe("ecs.bus.*.*.instance-register.*", sc.onRegister)
	if err != nil {
		return err
	}
	heartbeat, err := sc.nc.Subscribe("ecs.bus.*.*.instance-heartbeat.*", sc.onHeartbeat)
	if err != nil {
		_ = register.Unsubscribe()
		return err
	}
	taskState, err := sc.nc.Subscribe("ecs.bus.*.*.task-state.*", sc.onTaskState)
	if err != nil {
		_ = register.Unsubscribe()
		_ = heartbeat.Unsubscribe()
		return err
	}
	// The agent reports task state through the gateway, so the change lands on
	// whichever worker served the call. This is how it reaches the leader.
	wake, err := sc.nc.Subscribe("ecs.bus.*.*.service-reconcile", sc.onReconcileWake)
	if err != nil {
		_ = register.Unsubscribe()
		_ = heartbeat.Unsubscribe()
		_ = taskState.Unsubscribe()
		return err
	}
	sc.mu.Lock()
	sc.subs = []*nats.Subscription{register, heartbeat, taskState, wake}
	sc.mu.Unlock()
	return nil
}

// onReconcileWake carries no payload and writes nothing: the change it announces
// is already in KV by the time it is published.
func (sc *Scheduler) onReconcileWake(*nats.Msg) { sc.signal() }

func (sc *Scheduler) unsubscribeBus() {
	sc.mu.Lock()
	subs := sc.subs
	sc.subs = nil
	sc.mu.Unlock()
	for _, s := range subs {
		_ = s.Unsubscribe()
	}
}

func (sc *Scheduler) onRegister(msg *nats.Msg) {
	var m bus.RegisterInstance
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		slog.Warn("ECS scheduler: bad register payload", "err", err)
		return
	}
	// A bus callback outlives the Run context that wired its subscription, so it
	// binds its own — a cancelled ctx would abandon the KV write mid-record.
	if err := sc.svc.recordRegister(context.Background(), &m); err != nil {
		slog.Error("ECS scheduler: record register failed", "instance", m.InstanceID, "err", err)
		return
	}
	slog.Info("ECS scheduler: container instance registered", "cluster", m.ClusterName, "instance", m.InstanceID)
}

func (sc *Scheduler) onHeartbeat(msg *nats.Msg) {
	var m bus.Heartbeat
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		return
	}
	if err := sc.svc.recordHeartbeat(context.Background(), &m); err != nil {
		slog.Debug("ECS scheduler: record heartbeat failed", "instance", m.InstanceID, "err", err)
	}
}

func (sc *Scheduler) onTaskState(msg *nats.Msg) {
	var m bus.TaskState
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		return
	}
	if err := sc.svc.recordTaskState(context.Background(), &m); err != nil {
		slog.Error("ECS scheduler: record task-state failed", "task", m.TaskID, "err", err)
		return
	}
	// A task changing state is what moves a service's running count, so this is
	// the signal the reconcile was polling for. A heartbeat deliberately does not
	// signal: it says only that an instance is alive, which the reaper reads on
	// its own schedule.
	sc.signal()
	slog.Info("ECS scheduler: task state", "task", m.TaskID, "status", m.LastStatus)
}

// signal reports that the service reconcile has something to do, without
// blocking: the channel's single slot already means "at least one change is
// pending".
func (sc *Scheduler) signal() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// reap marks instances that have missed their heartbeat window DRAINING and stops
// their tasks, releasing capacity. Iterates every ECS account bucket. Returns an
// error when the account enumeration could not be completed, so a pass that
// could not see the whole fleet is reported rather than read as "nothing to
// reap" — every unlisted account keeps its dead instances' capacity held.
// The duration is when the soonest live instance's heartbeat window closes. An
// agent falling silent writes nothing, so a deadline is the only thing that
// brings the reaper back to notice it.
func (sc *Scheduler) reap(ctx context.Context) (time.Duration, error) {
	js, err := jetstream.New(sc.nc)
	if err != nil {
		return 0, err
	}
	buckets, err := accountBuckets(ctx, sc.nc)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var next time.Duration
	for _, bucket := range buckets {
		kv, err := js.KeyValue(ctx, bucket.name)
		if err != nil {
			slog.Error("ECS scheduler: open bucket failed", "bucket", bucket.name, "err", err)
			continue
		}
		next = reconciler.Earliest(next, sc.reapBucket(ctx, kv, bucket.accountID, now))
	}
	return next, nil
}

// reapBucket reaps one account's disconnected instances and reports when the
// soonest instance it left alone falls due.
func (sc *Scheduler) reapBucket(ctx context.Context, kv jetstream.KeyValue, accountID string, now time.Time) time.Duration {
	keys, err := keysWithPrefix(ctx, kv, "clusters/")
	if err != nil {
		return 0
	}
	var next time.Duration
	for _, k := range keys {
		if !strings.Contains(k, "/instances/") {
			continue
		}
		var inst InstanceRecord
		found, err := getJSON(ctx, kv, k, &inst)
		if err != nil || !found {
			continue
		}
		if inst.Status != InstanceStatusActive {
			continue
		}
		if now.Sub(inst.LastSeen) < heartbeatTimeout {
			next = reconciler.Earliest(next, inst.LastSeen.Add(heartbeatTimeout).Sub(now))
			continue
		}
		slog.Warn("ECS scheduler: reaping disconnected instance", "cluster", inst.Cluster, "instance", inst.InstanceID,
			"lastSeen", inst.LastSeen.Format(time.RFC3339))
		inst.Status = InstanceStatusDraining
		inst.Reaped = true
		if perr := putJSON(ctx, kv, k, &inst); perr != nil {
			continue
		}
		sc.stopInstanceTasks(ctx, kv, accountID, inst.Cluster, inst.InstanceID)
	}
	return next
}

// stopInstanceTasks transitions a reaped instance's non-stopped tasks to STOPPED
// and reclaims each awsvpc task's ENI (leak guard for a dead agent).
func (sc *Scheduler) stopInstanceTasks(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, instanceID string) {
	keys, err := keysWithPrefix(ctx, kv, TasksPrefix(cluster))
	if err != nil {
		return
	}
	for _, k := range keys {
		var task TaskRecord
		found, err := getJSON(ctx, kv, k, &task)
		if err != nil || !found {
			continue
		}
		if task.ContainerInstanceID != instanceID || task.LastStatus == TaskStatusStopped {
			continue
		}
		sc.svc.forceStopTask(ctx, kv, accountID, &task, stoppedReasonReaped)
	}
}

// convergeInstanceRoles re-asserts the ecsInstanceRole policy on every account
// that already holds the role. Provisioning runs only when capacity changes, so
// a cluster that is merely running would otherwise never pick up a changed
// document and its agent would fail its next credential refresh.
func (sc *Scheduler) convergeInstanceRoles(ctx context.Context) error {
	// A pass that cannot converge anything is a misconfiguration that silently
	// breaks agent credentials an hour later, so it is not a benign no-op.
	if sc.svc.deps.IAM == nil {
		return errors.New("instance-role converge disabled: IAM dependency not wired")
	}
	buckets, err := accountBuckets(ctx, sc.nc)
	if err != nil {
		return err
	}
	failed := 0
	for _, bucket := range buckets {
		if err := sc.svc.convergeECSInstanceRole(bucket.accountID); err != nil {
			failed++
			slog.Error("ECS scheduler: converge instance role failed",
				"accountId", bucket.accountID, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("converge instance roles: %d of %d accounts failed", failed, len(buckets))
	}
	return nil
}

// accountIDFromBucket extracts the account ID from an ECS per-account bucket name.
func accountIDFromBucket(bucket string) (string, bool) {
	if !strings.HasPrefix(bucket, KVBucketECSAccountPrefix) {
		return "", false
	}
	return strings.TrimPrefix(bucket, KVBucketECSAccountPrefix), true
}
