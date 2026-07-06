package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reloadService re-reads a service record from KV so a test sees writes made by
// the reconciler / failure accounting.
func reloadService(t *testing.T, kv nats.KeyValue, cluster, name string) *ServiceRecord {
	t.Helper()
	var rec ServiceRecord
	found, err := getJSON(kv, ServiceKey(cluster, name), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return &rec
}

// driveRunning reports RUNNING for every PENDING task of a service.
func driveRunning(t *testing.T, svc *Service, kv nats.KeyValue, cluster, name string) {
	t.Helper()
	tasks, err := svc.listServiceTasks(kv, cluster, name)
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
func failPending(t *testing.T, svc *Service, kv nats.KeyValue, cluster, name string) {
	t.Helper()
	tasks, err := svc.listServiceTasks(kv, cluster, name)
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
	tasks, err := svc.listServiceTasks(kv, "web", "web")
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
	tasks, err := svc.listServiceTasks(kv, "web", "web")
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
	tasks, err = svc.listServiceTasks(kv, "web", "web")
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
	tasks, err := svc.listServiceTasks(kv, "web", "web")
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
	require.NoError(t, putJSON(kv, ServiceKey("web", "legacy"), rec))

	require.NoError(t, svc.reconcileService(context.Background(), kv, testAccountID, rec))
	reloaded := reloadService(t, kv, "web", "legacy")
	primary := reloaded.primaryDeployment()
	require.NotNil(t, primary)
	assert.Equal(t, "legacy-dep", primary.ID)
	assert.Equal(t, defaultMinimumHealthyPercent, reloaded.MinimumHealthyPercent)
}
