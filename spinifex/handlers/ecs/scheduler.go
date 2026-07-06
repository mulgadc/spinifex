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
			if sc.isLeader() {
				sc.reap()
			}
		case <-reconcileTicker.C:
			if sc.isLeader() {
				sc.svc.reconcileAllServices()
			}
		case <-sweepTicker.C:
			if sc.isLeader() {
				sc.sweepStoppedTasks()
			}
		}
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
	won := sc.acquireOrRefresh()
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
func (sc *Scheduler) acquireOrRefresh() bool {
	js, err := sc.nc.JetStream()
	if err != nil {
		return false
	}
	kv, err := InitLeaderBucket(js)
	if err != nil {
		return false
	}
	if _, err := kv.Create(schedulerLeaderKey, []byte(sc.holder)); err == nil {
		return true
	}
	entry, err := kv.Get(schedulerLeaderKey)
	if err != nil {
		return false
	}
	if string(entry.Value()) != sc.holder {
		return false // someone else holds it
	}
	// We hold it — refresh to reset the TTL.
	if _, err := kv.Put(schedulerLeaderKey, []byte(sc.holder)); err != nil {
		return false
	}
	return true
}

// relinquish drops subscriptions and deletes the lease key on shutdown.
func (sc *Scheduler) relinquish() {
	sc.unsubscribeBus()
	js, err := sc.nc.JetStream()
	if err != nil {
		return
	}
	kv, err := InitLeaderBucket(js)
	if err != nil {
		return
	}
	if entry, gerr := kv.Get(schedulerLeaderKey); gerr == nil && string(entry.Value()) == sc.holder {
		_ = kv.Delete(schedulerLeaderKey)
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
	if err := sc.svc.recordRegister(&m); err != nil {
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
	if err := sc.svc.recordHeartbeat(&m); err != nil {
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
// their tasks, releasing capacity. Iterates every ECS account bucket.
func (sc *Scheduler) reap() {
	ctx := context.Background()
	js, err := sc.nc.JetStream()
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for bucket := range js.KeyValueStoreNames() {
		accountID, ok := accountIDFromBucket(bucket)
		if !ok {
			continue
		}
		kv, err := js.KeyValue(bucket)
		if err != nil {
			continue
		}
		sc.reapBucket(ctx, kv, accountID, now)
	}
}

func (sc *Scheduler) reapBucket(ctx context.Context, kv nats.KeyValue, accountID string, now time.Time) {
	keys, err := keysWithPrefix(kv, "clusters/")
	if err != nil {
		return
	}
	for _, k := range keys {
		if !strings.Contains(k, "/instances/") {
			continue
		}
		var inst InstanceRecord
		found, err := getJSON(kv, k, &inst)
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
		if perr := putJSON(kv, k, &inst); perr != nil {
			continue
		}
		sc.stopInstanceTasks(ctx, kv, accountID, inst.Cluster, inst.InstanceID)
	}
}

// stopInstanceTasks transitions a reaped instance's non-stopped tasks to STOPPED
// and reclaims each awsvpc task's ENI (leak guard for a dead agent).
func (sc *Scheduler) stopInstanceTasks(ctx context.Context, kv nats.KeyValue, accountID, cluster, instanceID string) {
	keys, err := keysWithPrefix(kv, TasksPrefix(cluster))
	if err != nil {
		return
	}
	for _, k := range keys {
		var task TaskRecord
		found, err := getJSON(kv, k, &task)
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
