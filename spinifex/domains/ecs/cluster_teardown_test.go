package ecs

import (
	"context"
	"github.com/mulgadc/spinifex/spinifex/domains/ecs/taskdefinition"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

	// Cluster subtree swept down to the INACTIVE meta record, which ListClusters
	// leaves out.
	list, err := svc.ListClusters(context.Background(), &ecs.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, list.ClusterArns)
	keys, err := taskdefinition.KeysWithPrefix(t.Context(), kv, clusterKeyPrefix("web"))
	require.NoError(t, err)
	assert.Equal(t, []string{ClusterMetaKey("web")}, keys)
}

// A deleted cluster still describes as INACTIVE with zeroed counts, as on AWS,
// and every other action treats it as not found.
func TestService_DeleteCluster_LeavesInactiveRecord(t *testing.T) {
	svc, _, _ := serviceTestRig(t)
	ctx := context.Background()
	_, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{
		ClusterName: aws.String("web"),
		Tags:        []*ecs.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.RunTask(ctx, &ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	_, err = svc.DeleteCluster(ctx, &ecs.DeleteClusterInput{Cluster: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 4096, 8192)

	out, err := svc.DescribeClusters(ctx, &ecs.DescribeClustersInput{
		Clusters: []*string{aws.String("web")}, Include: aws.StringSlice([]string{ecs.ClusterFieldTags}),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Failures)
	require.Len(t, out.Clusters, 1)
	c := out.Clusters[0]
	assert.Equal(t, ClusterStatusInactive, aws.StringValue(c.Status))
	assert.Equal(t, ClusterARN(testRegion, testAccountID, "web"), aws.StringValue(c.ClusterArn))
	assert.Zero(t, aws.Int64Value(c.RegisteredContainerInstancesCount))
	assert.Zero(t, aws.Int64Value(c.RunningTasksCount))
	assert.Zero(t, aws.Int64Value(c.PendingTasksCount))
	assert.Zero(t, aws.Int64Value(c.ActiveServicesCount))
	assert.Empty(t, c.Tags)

	arn := aws.String(ClusterARN(testRegion, testAccountID, "web"))
	cluster := aws.String("web")
	calls := map[string]func() error{
		"DeleteCluster": func() error {
			_, err := svc.DeleteCluster(ctx, &ecs.DeleteClusterInput{Cluster: cluster}, testAccountID)
			return err
		},
		"PutClusterCapacityProviders": func() error {
			_, err := svc.PutClusterCapacityProviders(ctx, &ecs.PutClusterCapacityProvidersInput{
				Cluster: cluster, CapacityProviders: []*string{}, DefaultCapacityProviderStrategy: []*ecs.CapacityProviderStrategyItem{},
			}, testAccountID)
			return err
		},
		"ListTagsForResource": func() error {
			_, err := svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: arn}, testAccountID)
			return err
		},
		"TagResource": func() error {
			_, err := svc.TagResource(ctx, &ecs.TagResourceInput{
				ResourceArn: arn, Tags: []*ecs.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
			}, testAccountID)
			return err
		},
		"CreateService": func() error {
			_, err := svc.CreateService(ctx, &ecs.CreateServiceInput{
				Cluster: cluster, ServiceName: aws.String("svc"), TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
			}, testAccountID)
			return err
		},
		"RunTask": func() error {
			_, err := svc.RunTask(ctx, &ecs.RunTaskInput{Cluster: cluster, TaskDefinition: aws.String("app")}, testAccountID)
			return err
		},
		"StartTask": func() error {
			_, err := svc.StartTask(ctx, &ecs.StartTaskInput{
				Cluster: cluster, TaskDefinition: aws.String("app"), ContainerInstances: aws.StringSlice([]string{"i-1"}),
			}, testAccountID)
			return err
		},
		"ListTasks": func() error {
			_, err := svc.ListTasks(ctx, &ecs.ListTasksInput{Cluster: cluster}, testAccountID)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			code, _, ok := awserrors.ResolveErrorDetail(call())
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorECSClusterNotFound, code)
		})
	}

	created, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: cluster}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, aws.StringValue(created.Cluster.Status))
	list, err := svc.ListClusters(ctx, &ecs.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []*string{arn}, list.ClusterArns)
}

func TestService_DeleteCluster_Unknown(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.DeleteCluster(context.Background(), &ecs.DeleteClusterInput{Cluster: aws.String("ghost")}, testAccountID)
	require.Error(t, err)
}

// cluster is required on DeleteCluster: omitting it must not resolve to, and
// delete, the default cluster.
func TestService_DeleteCluster_ClusterOmitted(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{}, testAccountID)
	require.NoError(t, err)

	for _, cluster := range []*string{nil, aws.String(""), aws.String("  ")} {
		_, err = svc.DeleteCluster(context.Background(), &ecs.DeleteClusterInput{Cluster: cluster}, testAccountID)
		code, _, ok := awserrors.ResolveErrorDetail(err)
		require.True(t, ok)
		assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
	}

	list, err := svc.ListClusters(context.Background(), &ecs.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, list.ClusterArns, 1)
}

func TestService_DeregisterTaskDefinition_InactivatesRevision(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 128, 256)

	out, err := svc.DeregisterTaskDefinition(context.Background(), &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: aws.String("app:1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, taskdefinition.StatusInactive, aws.StringValue(out.TaskDefinition.Status))

	// Still describable, now INACTIVE.
	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("app:1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, taskdefinition.StatusInactive, aws.StringValue(d.TaskDefinition.Status))
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
