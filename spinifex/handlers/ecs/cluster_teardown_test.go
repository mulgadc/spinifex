package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestService_DeleteCluster_CascadesAndSweeps(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DeleteCluster(context.Background(), &ecs.DeleteClusterInput{Cluster: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusInactive, aws.StringValue(out.Cluster.Status))

	// Whole cluster subtree swept: no clusters, no leftover task/service keys.
	list, err := svc.ListClusters(context.Background(), &ecs.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, list.ClusterArns)
	keys, err := keysWithPrefix(t.Context(), kv, clusterKeyPrefix("web"))
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestService_DeleteCluster_Unknown(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.DeleteCluster(context.Background(), &ecs.DeleteClusterInput{Cluster: aws.String("ghost")}, testAccountID)
	require.Error(t, err)
}

func TestService_DeregisterTaskDefinition_InactivatesRevision(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 128, 256)

	out, err := svc.DeregisterTaskDefinition(context.Background(), &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: aws.String("app:1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, TaskDefStatusInactive, aws.StringValue(out.TaskDefinition.Status))

	// Still describable, now INACTIVE.
	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("app:1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, TaskDefStatusInactive, aws.StringValue(d.TaskDefinition.Status))
}

func TestService_DeregisterTaskDefinition_RequiresRevision(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 128, 256)
	// Bare family (no revision) is rejected, matching AWS.
	_, err := svc.DeregisterTaskDefinition(context.Background(), &ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String("app")}, testAccountID)
	require.Error(t, err)
}

func TestService_DeregisterContainerInstance_ForceStopsAndRemoves(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)

	// Without force, an instance with a live task is rejected.
	_, err = svc.DeregisterContainerInstance(context.Background(), &ecs.DeregisterContainerInstanceInput{
		Cluster: aws.String("web"), ContainerInstance: aws.String("i-1"),
	}, testAccountID)
	require.Error(t, err)

	// With force, the task is stopped and the instance record removed.
	out, err := svc.DeregisterContainerInstance(context.Background(), &ecs.DeregisterContainerInstanceInput{
		Cluster: aws.String("web"), ContainerInstance: aws.String("i-1"), Force: aws.Bool(true),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusInactive, aws.StringValue(out.ContainerInstance.Status))

	active, err := svc.instanceActiveTasks(t.Context(), kv, "web", "i-1")
	require.NoError(t, err)
	assert.Empty(t, active)

	desc, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.ContainerInstances)
	require.Len(t, desc.Failures, 1)
}

func TestService_UpdateContainerInstancesState_DrainsServiceTasks(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.UpdateContainerInstancesState(context.Background(), &ecs.UpdateContainerInstancesStateInput{
		Cluster:            aws.String("web"),
		ContainerInstances: []*string{aws.String("i-1")},
		Status:             aws.String(InstanceStatusDraining),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, InstanceStatusDraining, aws.StringValue(out.ContainerInstances[0].Status))

	// Service-owned tasks on the drained instance are stopped.
	live, err := svc.listServiceTasks(t.Context(), kv, "web", "web")
	require.NoError(t, err)
	assert.Empty(t, live)
}

func TestService_UpdateContainerInstancesState_RejectsBadStatusAndUnknown(t *testing.T) {
	svc, _, _ := serviceTestRig(t)
	_, err := svc.UpdateContainerInstancesState(context.Background(), &ecs.UpdateContainerInstancesStateInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
		Status: aws.String("BOGUS"),
	}, testAccountID)
	require.Error(t, err)

	out, err := svc.UpdateContainerInstancesState(context.Background(), &ecs.UpdateContainerInstancesStateInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-ghost")},
		Status: aws.String(InstanceStatusActive),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.ContainerInstances)
	require.Len(t, out.Failures, 1)
}
