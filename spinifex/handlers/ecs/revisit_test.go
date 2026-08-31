//test:in-package — the deadlines are unexported pass returns (reap, timerPass,
//convergeIfDue) and the trigger is an unexported channel, so an external test
//package can reach neither.

package handlers_ecs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/mulgadc/spinifex/spinifex/reconciler"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// woke reports whether the scheduler asked for a service pass, and drains the
// signal so a later check sees only what happened after this one.
func woke(sc *Scheduler) bool {
	select {
	case <-sc.wake:
		return true
	default:
		return false
	}
}

func busMsg(t *testing.T, payload any) *nats.Msg {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	return &nats.Msg{Data: data}
}

// schedulerRig wires a scheduler over the shared service rig, with one cluster,
// one task definition and one registered instance.
func schedulerRig(t *testing.T) (*Scheduler, *Service, jetstream.KeyValue) {
	t.Helper()
	svc, nc := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 4096, 8192)
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	return NewScheduler(nc, svc, "test-holder"), svc, kv
}

// runOneTask places a task and returns its ID, read back from the record rather
// than sliced out of the ARN.
func runOneTask(t *testing.T, svc *Service, kv jetstream.KeyValue) string {
	t.Helper()
	_, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	keys, err := keysWithPrefix(t.Context(), kv, TasksPrefix("web"))
	require.NoError(t, err)
	require.Len(t, keys, 1)
	var task TaskRecord
	found, err := getJSON(t.Context(), kv, keys[0], &task)
	require.NoError(t, err)
	require.True(t, found)
	return task.TaskID
}

func TestScheduler_ATaskStateTriggersAPassAndAHeartbeatDoesNot(t *testing.T) {
	sc, svc, kv := schedulerRig(t)
	taskID := runOneTask(t, svc, kv)

	sc.onHeartbeat(busMsg(t, &bus.Heartbeat{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1",
	}))
	assert.False(t, woke(sc), "a heartbeat says only that an instance is alive: the reaper reads it, the reconcile does not")

	sc.onTaskState(busMsg(t, &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", TaskID: taskID, LastStatus: TaskStatusRunning,
	}))
	assert.True(t, woke(sc), "a task changing state moves a service's running count, so it must wake the reconcile")
}

func TestScheduler_ABurstOfTaskStatesLeavesOneSignal(t *testing.T) {
	sc, svc, kv := schedulerRig(t)
	taskID := runOneTask(t, svc, kv)

	for range 5 {
		sc.onTaskState(busMsg(t, &bus.TaskState{
			AccountID: testAccountID, ClusterName: "web", TaskID: taskID, LastStatus: TaskStatusRunning,
		}))
	}
	assert.True(t, woke(sc))
	assert.False(t, woke(sc), "the signal means 'something changed', so a burst must not queue five passes")
}

func TestScheduler_AnIdleServiceIsNotRewritten(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)

	// The first pass is allowed to settle the record; every pass after it has
	// nothing to say, and an unconditional write would churn KV forever.
	require.NoError(t, svc.reconcileAllServices(t.Context()))
	entry, err := kv.Get(t.Context(), ServiceKey("web", "web"))
	require.NoError(t, err)
	settled := entry.Revision()

	for range 3 {
		require.NoError(t, svc.reconcileAllServices(t.Context()))
	}
	entry, err = kv.Get(t.Context(), ServiceKey("web", "web"))
	require.NoError(t, err)
	assert.Equal(t, settled, entry.Revision(), "an idle service must not be rewritten by a pass that changed nothing")
}

func TestScheduler_TheReaperAsksForTheSoonestHeartbeatWindow(t *testing.T) {
	sc, _, kv := schedulerRig(t)
	now := time.Now().UTC()

	var inst InstanceRecord
	found, err := getJSON(t.Context(), kv, InstanceKey("web", "i-1"), &inst)
	require.NoError(t, err)
	require.True(t, found)
	inst.LastSeen = now.Add(-30 * time.Second)
	require.NoError(t, putJSON(t.Context(), kv, InstanceKey("web", "i-1"), &inst))

	due, err := sc.reap(t.Context())
	require.NoError(t, err)
	assert.InDelta(t, (heartbeatTimeout - 30*time.Second).Seconds(), due.Seconds(), 2,
		"a live instance falls due when its heartbeat window closes, not on the reaper's old interval")

	// Past the window the reaper acts, and with nothing live left it asks for
	// nothing: the resync is the only thing that need bring it back.
	inst.LastSeen = now.Add(-2 * heartbeatTimeout)
	require.NoError(t, putJSON(t.Context(), kv, InstanceKey("web", "i-1"), &inst))
	due, err = sc.reap(t.Context())
	require.NoError(t, err)
	assert.Zero(t, due)

	found, err = getJSON(t.Context(), kv, InstanceKey("web", "i-1"), &inst)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, InstanceStatusDraining, inst.Status, "an instance past its window is still reaped")
}

func TestScheduler_ConvergeIsDueAtItsIntervalAndNotBefore(t *testing.T) {
	sc, _, _ := schedulerRig(t)

	// Unstamped, so the first pass runs it and asks for the full interval.
	due := sc.convergeIfDue(t.Context())
	assert.Equal(t, convergeInterval, due)
	first := sc.lastConverge
	require.False(t, first.IsZero())

	due = sc.convergeIfDue(t.Context())
	assert.Positive(t, due)
	assert.LessOrEqual(t, due, convergeInterval)
	assert.Equal(t, first, sc.lastConverge, "a pass inside the interval must not re-assert the policy")

	sc.lastConverge = time.Now().Add(-2 * convergeInterval)
	due = sc.convergeIfDue(t.Context())
	assert.Equal(t, convergeInterval, due)
	assert.True(t, sc.lastConverge.After(first))
}

func TestScheduler_AFollowerAsksAgainAtTheLeaseRate(t *testing.T) {
	sc, _, _ := schedulerRig(t)
	require.False(t, sc.lease.Held())

	// Leadership turns over without anything being written, so a follower has no
	// signal to wait on and must keep asking.
	due, err := sc.reconcileServicesPass(t.Context())
	require.NoError(t, err)
	assert.Equal(t, leaseRefresh, due)

	due, err = sc.timerPass(t.Context())
	require.NoError(t, err)
	assert.Equal(t, leaseRefresh, due)
}

func TestScheduler_TheTimerPassReturnsTheSoonestOfItsJobs(t *testing.T) {
	sc, _, kv := schedulerRig(t)
	now := time.Now().UTC()

	// A live instance due in 10s, and a stopped task due much later: the pass must
	// come back for the sooner of the two.
	var inst InstanceRecord
	found, err := getJSON(t.Context(), kv, InstanceKey("web", "i-1"), &inst)
	require.NoError(t, err)
	require.True(t, found)
	inst.LastSeen = now.Add(10*time.Second - heartbeatTimeout)
	require.NoError(t, putJSON(t.Context(), kv, InstanceKey("web", "i-1"), &inst))

	stopped := TaskRecord{
		TaskID: "t-1", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "t-1"),
		LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped, StoppedAt: now,
	}
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &stopped))

	reapDue, err := sc.reap(t.Context())
	require.NoError(t, err)
	sweepDue, err := sc.sweepStoppedTasks(t.Context())
	require.NoError(t, err)
	require.Positive(t, sweepDue)
	assert.Equal(t, reapDue, reconciler.Earliest(reapDue, sweepDue),
		"the pass returns the soonest deadline, not the last one computed")
	assert.InDelta(t, 10.0, reapDue.Seconds(), 2)
}
