package handlers_ecs

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
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
	reconcileInterval   = 10 * time.Second
	heartbeatTimeout    = 90 * time.Second
	stoppedReasonReaped = "ContainerInstance disconnected"

	// sweepInterval is how often the leader prunes stale STOPPED task records;
	// stoppedTaskRetention keeps a just-stopped task describable (DescribeTasks /
	// UI exit reason) before it is dropped, matching AWS's ~1h STOPPED window.
	sweepInterval        = 60 * time.Second
	stoppedTaskRetention = 1 * time.Hour
)

// Scheduler is the per-daemon ECS control loop. A single leader (elected via the
// shared leader bucket) owns the Layer-2 bus subscriptions and the heartbeat
// reaper; losers idle and retry. Placement itself happens synchronously in the
// RunTask handler, so a brief leaderless gap never blocks RunTask.
type Scheduler struct {
	nc     *nats.Conn
	svc    *Service
	holder string

	mu     sync.Mutex
	leader bool
	subs   []*nats.Subscription
}

// NewScheduler constructs a Scheduler. holder identifies this daemon in the lease.
func NewScheduler(nc *nats.Conn, svc *Service, holder string) *Scheduler {
	return &Scheduler{nc: nc, svc: svc, holder: holder}
}

// Run drives the leadership + reaper loop until ctx is cancelled. It is intended
// to run as a daemon-boot goroutine; panics are the caller's recover concern.
func (sc *Scheduler) Run(ctx context.Context) {
	leaseTicker := time.NewTicker(leaseRefresh)
	reaperTicker := time.NewTicker(reaperInterval)
	reconcileTicker := time.NewTicker(reconcileInterval)
	sweepTicker := time.NewTicker(sweepInterval)
	defer leaseTicker.Stop()
	defer reaperTicker.Stop()
	defer reconcileTicker.Stop()
	defer sweepTicker.Stop()

	sc.evaluateLeadership(ctx)
	for {
		select {
		case <-ctx.Done():
			sc.relinquish()
			return
		case <-leaseTicker.C:
			sc.evaluateLeadership(ctx)
		case <-reaperTicker.C:
			sc.runIfLeader("instance reap", func() error { return sc.reap(ctx) })
		case <-reconcileTicker.C:
			sc.runIfLeader("service reconcile", func() error { return sc.svc.reconcileAllServices(ctx) })
		case <-sweepTicker.C:
			sc.runIfLeader("stopped-task sweep", func() error { return sc.sweepStoppedTasks(ctx) })
		}
	}
}

// runIfLeader runs one leader-only pass, logging a pass that could not complete.
// A pass errors only when it could not observe the whole fleet, so this log is
// the operator's signal that a tick was skipped rather than found nothing to do.
func (sc *Scheduler) runIfLeader(pass string, run func() error) {
	if !sc.isLeader() {
		return
	}
	if err := run(); err != nil {
		slog.Error("ECS scheduler: pass failed", "pass", pass, "err", err)
	}
}

func (sc *Scheduler) isLeader() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.leader
}

// evaluateLeadership acquires or refreshes the lease, wiring up (or tearing down)
// the bus subscriptions as leadership changes.
func (sc *Scheduler) evaluateLeadership(ctx context.Context) {
	won := sc.acquireOrRefresh(ctx)
	sc.mu.Lock()
	was := sc.leader
	sc.leader = won
	sc.mu.Unlock()

	switch {
	case won && !was:
		if err := sc.subscribeBus(ctx); err != nil {
			slog.Error("ECS scheduler: bus subscribe failed", "holder", sc.holder, "err", err)
		} else {
			slog.Info("ECS scheduler: elected leader, bus subscriptions active", "holder", sc.holder)
		}
	case !won && was:
		sc.unsubscribeBus()
		slog.Info("ECS scheduler: lost leadership, bus subscriptions dropped", "holder", sc.holder)
	}
}

// acquireOrRefresh tries to claim the scheduler lease, refreshing it (resetting
// the TTL) when we already hold it. Returns true when we are the leader.
func (sc *Scheduler) acquireOrRefresh(ctx context.Context) bool {
	js, err := jetstream.New(sc.nc)
	if err != nil {
		return false
	}
	kv, err := InitLeaderBucket(ctx, js)
	if err != nil {
		return false
	}
	if _, err := kv.Create(ctx, schedulerLeaderKey, []byte(sc.holder)); err == nil {
		return true
	}
	entry, err := kv.Get(ctx, schedulerLeaderKey)
	if err != nil {
		return false
	}
	if string(entry.Value()) != sc.holder {
		return false // someone else holds it
	}
	// We hold it — refresh to reset the TTL.
	if _, err := kv.Put(ctx, schedulerLeaderKey, []byte(sc.holder)); err != nil {
		return false
	}
	return true
}

// relinquish drops subscriptions and deletes the lease key on shutdown.
func (sc *Scheduler) relinquish() {
	sc.unsubscribeBus()
	// Run's ctx is already cancelled by the time it calls relinquish, so the
	// release runs on its own context — a captured ctx would fail the delete and
	// leave the lease for the TTL to reap.
	ctx := context.Background()
	js, err := jetstream.New(sc.nc)
	if err != nil {
		return
	}
	kv, err := InitLeaderBucket(ctx, js)
	if err != nil {
		return
	}
	if entry, gerr := kv.Get(ctx, schedulerLeaderKey); gerr == nil && string(entry.Value()) == sc.holder {
		_ = kv.Delete(ctx, schedulerLeaderKey)
	}
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
	sc.mu.Lock()
	sc.subs = []*nats.Subscription{register, heartbeat, taskState}
	sc.mu.Unlock()
	return nil
}

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
	slog.Info("ECS scheduler: task state", "task", m.TaskID, "status", m.LastStatus)
}

// reap marks instances that have missed their heartbeat window DRAINING and stops
// their tasks, releasing capacity. Iterates every ECS account bucket. Returns an
// error when the account enumeration could not be completed, so a pass that
// could not see the whole fleet is reported rather than read as "nothing to
// reap" — every unlisted account keeps its dead instances' capacity held.
func (sc *Scheduler) reap(ctx context.Context) error {
	js, err := jetstream.New(sc.nc)
	if err != nil {
		return err
	}
	buckets, err := accountBuckets(ctx, sc.nc)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, bucket := range buckets {
		kv, err := js.KeyValue(ctx, bucket.name)
		if err != nil {
			slog.Error("ECS scheduler: open bucket failed", "bucket", bucket.name, "err", err)
			continue
		}
		sc.reapBucket(ctx, kv, bucket.accountID, now)
	}
	return nil
}

func (sc *Scheduler) reapBucket(ctx context.Context, kv jetstream.KeyValue, accountID string, now time.Time) {
	keys, err := keysWithPrefix(ctx, kv, "clusters/")
	if err != nil {
		return
	}
	for _, k := range keys {
		if !strings.Contains(k, "/instances/") {
			continue
		}
		var inst InstanceRecord
		found, err := getJSON(ctx, kv, k, &inst)
		if err != nil || !found {
			continue
		}
		if inst.Status != InstanceStatusActive || now.Sub(inst.LastSeen) < heartbeatTimeout {
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

// accountIDFromBucket extracts the account ID from an ECS per-account bucket name.
func accountIDFromBucket(bucket string) (string, bool) {
	if !strings.HasPrefix(bucket, KVBucketECSAccountPrefix) {
		return "", false
	}
	return strings.TrimPrefix(bucket, KVBucketECSAccountPrefix), true
}
