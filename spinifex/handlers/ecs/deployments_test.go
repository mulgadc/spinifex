package handlers_ecs

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reloadService re-reads a service record from KV so a test sees writes made by
// the reconciler / failure accounting.
func reloadService(t *testing.T, kv jetstream.KeyValue, cluster, name string) *ServiceRecord {
	t.Helper()
	var rec ServiceRecord
	found, err := getJSON(t.Context(), kv, ServiceKey(cluster, name), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return &rec
}

// driveRunning reports RUNNING for every PENDING task of a service.
func driveRunning(t *testing.T, svc *Service, kv jetstream.KeyValue, cluster, name string) {
	t.Helper()
	tasks, err := svc.listServiceTasks(t.Context(), kv, cluster, name)
	require.NoError(t, err)
	for i := range tasks {
		if tasks[i].LastStatus != TaskStatusPending {
			continue
		}
		require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
			AccountID: testAccountID, ClusterName: cluster, TaskID: tasks[i].TaskID,
			LastStatus: TaskStatusRunning,
		}))
	}
}

// failPending reports STOPPED (never-RUNNING) for every PENDING task, simulating
// tasks that fail to start — the deployment circuit-breaker signal.
func failPending(t *testing.T, svc *Service, kv jetstream.KeyValue, cluster, name string) {
	t.Helper()
	tasks, err := svc.listServiceTasks(t.Context(), kv, cluster, name)
	require.NoError(t, err)
	for i := range tasks {
		if tasks[i].LastStatus != TaskStatusPending {
			continue
		}
		require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
			AccountID: testAccountID, ClusterName: cluster, TaskID: tasks[i].TaskID,
			LastStatus: TaskStatusStopped, Reason: "image pull failed",
		}))
	}
}

func TestDeployment_CreateService_SeedsPrimary(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	out, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)

	require.Len(t, out.Service.Deployments, 1)
	assert.Equal(t, DeploymentStatusPrimary, aws.StringValue(out.Service.Deployments[0].Status))
	assert.Equal(t, RolloutStateInProgress, aws.StringValue(out.Service.Deployments[0].RolloutState))

	rec := reloadService(t, kv, "web", "web")
	require.NotNil(t, rec.primaryDeployment())
	assert.Equal(t, defaultMinimumHealthyPercent, rec.MinimumHealthyPercent)
	assert.Equal(t, defaultMaximumPercent, rec.MaximumPercent)
	// Every launched task carries the primary deployment id in StartedBy.
	tasks, err := svc.listServiceTasks(t.Context(), kv, "web", "web")
	require.NoError(t, err)
	require.Len(t, tasks, 2)
	for i := range tasks {
		assert.Equal(t, rec.DeploymentID, deploymentIDFromStartedBy(tasks[i].StartedBy))
	}
}

func TestDeployment_RollingUpdate_ReplacesOldWithNew(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)

	// Complete the initial deployment.
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	rec := reloadService(t, kv, "web", "web")
	require.Len(t, rec.Deployments, 1)
	assert.Equal(t, RolloutStateCompleted, rec.primaryDeployment().RolloutState)
	firstDeployID := rec.DeploymentID

	// New taskdef revision starts a rolling deployment.
	registerTaskDef(t, svc, "app", 128, 256) // app:2
	upd, err := svc.UpdateService(context.Background(), &ecs.UpdateServiceInput{
		Cluster: aws.String("web"), Service: aws.String("web"),
		TaskDefinition: aws.String("app:2"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, upd.Service.Deployments, 2) // PRIMARY app:2 + ACTIVE app:1

	rec = reloadService(t, kv, "web", "web")
	assert.NotEqual(t, firstDeployID, rec.DeploymentID)
	// maximumPercent=200 lets 2 new tasks launch alongside the 2 old running ones.
	newTasks := 0
	tasks, err := svc.listServiceTasks(t.Context(), kv, "web", "web")
	require.NoError(t, err)
	for i := range tasks {
		if deploymentIDFromStartedBy(tasks[i].StartedBy) == rec.DeploymentID {
			newTasks++
		}
	}
	assert.Equal(t, 2, newTasks)

	// New tasks come up; a reconcile drains the old ones and completes the rollout.
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	rec = reloadService(t, kv, "web", "web")
	require.Len(t, rec.Deployments, 1)
	assert.Equal(t, RolloutStateCompleted, rec.primaryDeployment().RolloutState)
	assert.Equal(t, rec.DeploymentID, rec.primaryDeployment().ID)

	// Only new-deployment tasks remain live.
	tasks, err = svc.listServiceTasks(t.Context(), kv, "web", "web")
	require.NoError(t, err)
	require.Len(t, tasks, 2)
	for i := range tasks {
		assert.Equal(t, rec.DeploymentID, deploymentIDFromStartedBy(tasks[i].StartedBy))
	}
}

func TestDeployment_MinimumHealthyPercent_GatesDrain(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	// min=100, max=150: with desired=2 the rollout may run up to 3 tasks and must
	// keep 2 healthy, so old tasks only drain as new ones become healthy.
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(2),
		DeploymentConfiguration: &ecs.DeploymentConfiguration{
			MinimumHealthyPercent: aws.Int64(100), MaximumPercent: aws.Int64(150),
		},
	}, testAccountID)
	require.NoError(t, err)
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))

	registerTaskDef(t, svc, "app", 128, 256) // app:2
	_, err = svc.UpdateService(context.Background(), &ecs.UpdateServiceInput{
		Cluster: aws.String("web"), Service: aws.String("web"), TaskDefinition: aws.String("app:2"),
	}, testAccountID)
	require.NoError(t, err)

	// Ceiling of 3 tasks total: 2 old running + 1 new pending. No old task drained
	// yet because healthy running (2) must not drop below minCount (2).
	tasks, err := svc.listServiceTasks(t.Context(), kv, "web", "web")
	require.NoError(t, err)
	assert.Len(t, tasks, 3)
	rec := reloadService(t, kv, "web", "web")
	assert.Equal(t, 2, rec.RunningCount)
	assert.Equal(t, 1, rec.PendingCount)
}

func TestDeployment_CircuitBreaker_RollsBackToLastGood(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
		DeploymentConfiguration: &ecs.DeploymentConfiguration{
			DeploymentCircuitBreaker: &ecs.DeploymentCircuitBreaker{
				Enable: aws.Bool(true), Rollback: aws.Bool(true),
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	goodARN := reloadService(t, kv, "web", "web").LastGoodTaskDefARN
	require.NotEmpty(t, goodARN)

	// Roll out a revision whose tasks always fail to start.
	registerTaskDef(t, svc, "app", 128, 256) // app:2
	_, err = svc.UpdateService(context.Background(), &ecs.UpdateServiceInput{
		Cluster: aws.String("web"), Service: aws.String("web"), TaskDefinition: aws.String("app:2"),
	}, testAccountID)
	require.NoError(t, err)

	// Each cycle: fail the pending new task, then reconcile relaunches. After the
	// failure threshold the breaker trips on the next reconcile and rolls back.
	for range circuitBreakerFailureThreshold {
		failPending(t, svc, kv, "web", "web")
		require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	}

	rec := reloadService(t, kv, "web", "web")
	primary := rec.primaryDeployment()
	require.NotNil(t, primary)
	assert.Equal(t, goodARN, primary.TaskDefARN)
	assert.Equal(t, goodARN, rec.TaskDefARN)
}

// TestDeployment_Reconcile_RepeatedPassesDoNotGrowEventRing is the guard for
// the hazard the ECS service-events plan calls out: the reconcile loop runs
// repeatedly, so a naive unconditional append on every pass would fill the
// ring with the same "reached a steady state" message and evict every
// genuinely informative event before anyone reads it. Emission must be
// edge-triggered on the rollout-state transition, not the reconcile pass.
func TestDeployment_Reconcile_RepeatedPassesDoNotGrowEventRing(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))

	rec := reloadService(t, kv, "web", "web")
	require.Len(t, rec.Events, 1)
	assert.Contains(t, rec.Events[0].Message, "reached a steady state")

	for range 20 {
		require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	}
	rec = reloadService(t, kv, "web", "web")
	assert.Len(t, rec.Events, 1, "repeated reconcile over an unchanged steady service must not grow the ring")
}

// TestDeployment_SteadyState_EntersOnceLeavesAndReentersAppendsSecond covers
// the edge-triggered emission requirement directly: entering steady state
// appends exactly one event, and a rollout that leaves steady state (a new
// task definition) and completes again appends a second, distinct entry.
func TestDeployment_SteadyState_EntersOnceLeavesAndReentersAppendsSecond(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))

	rec := reloadService(t, kv, "web", "web")
	require.Len(t, rec.Events, 1)

	// A new task definition starts a fresh rollout: leaves steady state, and
	// must not itself append (only entering/reaching steady state does).
	registerTaskDef(t, svc, "app", 128, 256) // app:2
	_, err = svc.UpdateService(context.Background(), &ecs.UpdateServiceInput{
		Cluster: aws.String("web"), Service: aws.String("web"), TaskDefinition: aws.String("app:2"),
	}, testAccountID)
	require.NoError(t, err)
	rec = reloadService(t, kv, "web", "web")
	require.Equal(t, RolloutStateInProgress, rec.primaryDeployment().RolloutState)
	assert.Len(t, rec.Events, 1, "leaving steady state must not append an event")

	// The new task comes up and the rollout completes: re-entering steady
	// state appends a second, distinct event.
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	rec = reloadService(t, kv, "web", "web")
	require.Len(t, rec.Events, 2)
	assert.Contains(t, rec.Events[0].Message, "reached a steady state")
}

// TestDeployment_CircuitBreaker_EventCarriesFailureReason covers the final
// required transition: a circuit-breaker trip appends an event, and the event
// carries the same failure reason recorded on the deployment.
func TestDeployment_CircuitBreaker_EventCarriesFailureReason(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
		DeploymentConfiguration: &ecs.DeploymentConfiguration{
			DeploymentCircuitBreaker: &ecs.DeploymentCircuitBreaker{
				Enable: aws.Bool(true), Rollback: aws.Bool(false),
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	driveRunning(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))

	registerTaskDef(t, svc, "app", 128, 256) // app:2
	_, err = svc.UpdateService(context.Background(), &ecs.UpdateServiceInput{
		Cluster: aws.String("web"), Service: aws.String("web"), TaskDefinition: aws.String("app:2"),
	}, testAccountID)
	require.NoError(t, err)

	for range circuitBreakerFailureThreshold {
		failPending(t, svc, kv, "web", "web")
		require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	}

	rec := reloadService(t, kv, "web", "web")
	primary := rec.primaryDeployment()
	require.NotNil(t, primary)
	require.Equal(t, RolloutStateFailed, primary.RolloutState)
	require.NotEmpty(t, rec.Events)
	assert.Contains(t, rec.Events[0].Message, "deployment failed")
	assert.Contains(t, rec.Events[0].Message, primary.RolloutReason)

	// The DescribeServices projection carries the same event, newest first,
	// alongside the earlier steady-state entry from the first deployment.
	desc, err := svc.DescribeServices(context.Background(), &ecs.DescribeServicesInput{
		Cluster: aws.String("web"), Services: []*string{aws.String("web")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Services, 1)
	require.NotEmpty(t, desc.Services[0].Events)
	assert.Contains(t, aws.StringValue(desc.Services[0].Events[0].Message), "deployment failed")
}

func TestDeployment_LegacyServiceSynthesizesPrimary(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	// A record written before deployment tracking existed: no Deployments slice.
	rec := &ServiceRecord{
		Name: "legacy", ARN: ServiceARN(testRegion, testAccountID, "web", "legacy"),
		Cluster: "web", TaskDefFamily: "app", TaskDefRevision: 1,
		TaskDefARN:   TaskDefARN(testRegion, testAccountID, "app", 1),
		DesiredCount: 1, Status: ServiceStatusActive,
		SchedulingStrategy: SchedulingStrategyReplica, DeploymentID: "legacy-dep",
	}
	require.NoError(t, putJSON(t.Context(), kv, ServiceKey("web", "legacy"), rec))

	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, rec))
	reloaded := reloadService(t, kv, "web", "legacy")
	primary := reloaded.primaryDeployment()
	require.NotNil(t, primary)
	assert.Equal(t, "legacy-dep", primary.ID)
	assert.Equal(t, defaultMinimumHealthyPercent, reloaded.MinimumHealthyPercent)
}

// TestLaunchBackoff_WidensWithFailuresAndCaps is the arithmetic on its own: no
// hold while the circuit breaker is still counting, then doubling, then a cap so
// a long-dead deployment does not wait forever to be retried.
func TestLaunchBackoff_WidensWithFailuresAndCaps(t *testing.T) {
	for failures := range circuitBreakerFailureThreshold {
		assert.Zero(t, launchBackoff(failures), "held off at %d failures, below the breaker threshold", failures)
	}
	assert.Equal(t, launchBackoffBase, launchBackoff(circuitBreakerFailureThreshold))
	assert.Equal(t, 2*launchBackoffBase, launchBackoff(circuitBreakerFailureThreshold+1))
	assert.Equal(t, 4*launchBackoffBase, launchBackoff(circuitBreakerFailureThreshold+2))
	assert.Equal(t, launchBackoffCap, launchBackoff(circuitBreakerFailureThreshold+20))
	assert.Equal(t, launchBackoffCap, launchBackoff(1_000_000))
}

// A deployment whose tasks keep dying on start stops relaunching on every pass.
// AWS leaves the deployment circuit breaker off by default, so without this a
// service whose node cannot run containers churns tasks forever — which is how
// one env accumulated hundreds of STOPPED tasks at one every thirty seconds.
func TestDeployment_RepeatedStartFailuresBackOffWithoutTheCircuitBreaker(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)

	// Fail every task the reconciler launches. The breaker is off, so nothing
	// else brakes this.
	for range circuitBreakerFailureThreshold {
		failPending(t, svc, kv, "web", "web")
		require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	}

	rec := reloadService(t, kv, "web", "web")
	primary := rec.primaryDeployment()
	require.NotNil(t, primary)
	require.GreaterOrEqual(t, primary.FailedTasks, circuitBreakerFailureThreshold)
	require.False(t, primary.NextLaunchAt.IsZero(), "no backoff recorded after repeated start failures")
	assert.True(t, primary.NextLaunchAt.After(time.Now().UTC()), "backoff deadline is already in the past")

	// The service is below its desired count and would otherwise launch, but the
	// deadline holds the pass off.
	failPending(t, svc, kv, "web", "web")
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))
	assert.Zero(t, liveServiceTasks(t, svc, kv, "web", "web"), "reconcile launched a task while the deployment was backed off")

	// Once it passes, launching resumes: the backoff is a brake, not a stop.
	rec = reloadService(t, kv, "web", "web")
	rec.primaryDeployment().NextLaunchAt = time.Now().UTC().Add(-time.Second)
	require.NoError(t, putJSON(t.Context(), kv, ServiceKey("web", "web"), rec))
	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, reloadService(t, kv, "web", "web")))

	assert.Positive(t, liveServiceTasks(t, svc, kv, "web", "web"), "backoff never released the deployment")
}

// liveServiceTasks counts a service's tasks that have not stopped, which is what
// the reconciler is trying to hold at the desired count.
func liveServiceTasks(t *testing.T, svc *Service, kv jetstream.KeyValue, cluster, name string) int {
	t.Helper()
	tasks, err := svc.listServiceTasks(t.Context(), kv, cluster, name)
	require.NoError(t, err)
	n := 0
	for i := range tasks {
		if tasks[i].LastStatus != TaskStatusStopped {
			n++
		}
	}
	return n
}
