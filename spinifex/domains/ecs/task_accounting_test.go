package ecs_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	ecsv1 "github.com/mulgadc/spinifex/contracts/ecs/v1"
	ecsdomain "github.com/mulgadc/spinifex/spinifex/domains/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTasks writes task records straight to KV. The statuses under test are
// reported by the agent, so driving them through RunTask would require a live
// agent to reach anything other than PENDING.
func seedTasks(t *testing.T, svc *ecsdomain.Service, cluster string, recs ...*ecsdomain.TaskRecord) {
	t.Helper()
	_, err := svc.CreateCluster(t.Context(), &ecs.CreateClusterInput{ClusterName: aws.String(cluster)}, ecsdomain.TestAccountID)
	require.NoError(t, err)
	kv, err := svc.Bucket(t.Context(), ecsdomain.TestAccountID)
	require.NoError(t, err)
	for _, r := range recs {
		r.Cluster = cluster
		r.ARN = ecsdomain.TaskARN(svc.Region(), ecsdomain.TestAccountID, cluster, r.TaskID)
		require.NoError(t, ecsdomain.PutJSON(t.Context(), kv, ecsdomain.TaskKey(cluster, r.TaskID), r))
	}
}

func listTaskIDs(t *testing.T, svc *ecsdomain.Service, input *ecs.ListTasksInput) []string {
	t.Helper()
	out, err := svc.ListTasks(t.Context(), input, ecsdomain.TestAccountID)
	require.NoError(t, err)
	ids := make([]string, 0, len(out.TaskArns))
	for _, a := range out.TaskArns {
		ids = append(ids, ecsdomain.ContainerInstanceShortID(aws.StringValue(a)))
	}
	return ids
}

func TestListTasks_DesiredStatusFilters(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	seedTasks(t, svc, "web",
		&ecsdomain.TaskRecord{TaskID: "run-1", DesiredStatus: ecsdomain.TaskStatusRunning, LastStatus: ecsdomain.TaskStatusRunning},
		&ecsdomain.TaskRecord{TaskID: "stop-1", DesiredStatus: ecsdomain.TaskStatusStopped, LastStatus: ecsdomain.TaskStatusStopped},
		&ecsdomain.TaskRecord{TaskID: "stop-2", DesiredStatus: ecsdomain.TaskStatusStopped, LastStatus: ecsdomain.TaskStatusStopped},
	)

	assert.ElementsMatch(t, []string{"run-1"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web"), DesiredStatus: aws.String(ecsdomain.TaskStatusRunning)}))
	assert.ElementsMatch(t, []string{"stop-1", "stop-2"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web"), DesiredStatus: aws.String(ecsdomain.TaskStatusStopped)}))
}

// The default is the case that broke a caller: asking for nothing used to hand
// back every task the cluster had ever run.
func TestListTasks_OmittedDesiredStatusMeansRunning(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	seedTasks(t, svc, "web",
		&ecsdomain.TaskRecord{TaskID: "run-1", DesiredStatus: ecsdomain.TaskStatusRunning, LastStatus: ecsdomain.TaskStatusRunning},
		&ecsdomain.TaskRecord{TaskID: "stop-1", DesiredStatus: ecsdomain.TaskStatusStopped, LastStatus: ecsdomain.TaskStatusStopped},
	)

	assert.ElementsMatch(t, []string{"run-1"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web")}))
}

func TestListTasks_FamilyStartedByServiceAndInstanceFilter(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	seedTasks(t, svc, "web",
		&ecsdomain.TaskRecord{
			TaskID: "a", DesiredStatus: ecsdomain.TaskStatusRunning, LastStatus: ecsdomain.TaskStatusRunning,
			TaskDefFamily: "api", StartedBy: "ci", Group: ecsdomain.ServiceTaskGroup("web"), ContainerInstanceID: "i-1",
		},
		&ecsdomain.TaskRecord{
			TaskID: "b", DesiredStatus: ecsdomain.TaskStatusRunning, LastStatus: ecsdomain.TaskStatusRunning,
			TaskDefFamily: "worker", StartedBy: "human", Group: ecsdomain.ServiceTaskGroup("batch"), ContainerInstanceID: "i-2",
		},
	)

	cluster := aws.String("web")
	assert.ElementsMatch(t, []string{"a"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: cluster, Family: aws.String("api")}))
	assert.ElementsMatch(t, []string{"b"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: cluster, StartedBy: aws.String("human")}))
	assert.ElementsMatch(t, []string{"a"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: cluster, ServiceName: aws.String("web")}))
	assert.ElementsMatch(t, []string{"b"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: cluster, ContainerInstance: aws.String("i-2")}))
}

// Filters combine, so a task has to satisfy all of them rather than any.
func TestListTasks_FiltersCombine(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	seedTasks(t, svc, "web",
		&ecsdomain.TaskRecord{
			TaskID: "a", DesiredStatus: ecsdomain.TaskStatusRunning, LastStatus: ecsdomain.TaskStatusRunning,
			TaskDefFamily: "api", ContainerInstanceID: "i-1",
		},
		&ecsdomain.TaskRecord{
			TaskID: "b", DesiredStatus: ecsdomain.TaskStatusRunning, LastStatus: ecsdomain.TaskStatusRunning,
			TaskDefFamily: "api", ContainerInstanceID: "i-2",
		},
	)

	assert.ElementsMatch(t, []string{"a"}, listTaskIDs(t, svc, &ecs.ListTasksInput{
		Cluster: aws.String("web"), Family: aws.String("api"), ContainerInstance: aws.String("i-1"),
	}))
}

// A task nobody asked to stop still has to leave the RUNNING filter when it
// exits, or a caller polling for live tasks never stops seeing it.
func TestRecordTaskState_SelfExitMovesDesiredStatusToStopped(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	seedTasks(t, svc, "web",
		&ecsdomain.TaskRecord{TaskID: "t-1", DesiredStatus: ecsdomain.TaskStatusRunning, LastStatus: ecsdomain.TaskStatusRunning},
	)

	require.NoError(t, svc.RecordTaskState(t.Context(), &ecsv1.TaskState{
		AccountID: ecsdomain.TestAccountID, ClusterName: "web", TaskID: "t-1",
		LastStatus: ecsdomain.TaskStatusStopped, Reason: "container exited (1)",
	}))

	kv, err := svc.Bucket(t.Context(), ecsdomain.TestAccountID)
	require.NoError(t, err)
	var rec ecsdomain.TaskRecord
	found, err := ecsdomain.GetJSON(t.Context(), kv, ecsdomain.TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ecsdomain.TaskStatusStopped, rec.DesiredStatus)

	assert.Empty(t, listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web")}))
	assert.ElementsMatch(t, []string{"t-1"}, listTaskIDs(t, svc, &ecs.ListTasksInput{
		Cluster: aws.String("web"), DesiredStatus: aws.String(ecsdomain.TaskStatusStopped),
	}))
}

func TestDescribeContainerInstances_CountsComeFromTaskRecords(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	_, err := svc.RegisterContainerInstance(t.Context(), &ecs.RegisterContainerInstanceInput{
		Cluster:                  aws.String("web"),
		InstanceIdentityDocument: aws.String("i-1"),
	}, ecsdomain.TestAccountID)
	require.NoError(t, err)

	seedTasks(t, svc, "web",
		&ecsdomain.TaskRecord{TaskID: "r1", LastStatus: ecsdomain.TaskStatusRunning, ContainerInstanceID: "i-1"},
		&ecsdomain.TaskRecord{TaskID: "p1", LastStatus: ecsdomain.TaskStatusPending, ContainerInstanceID: "i-1"},
		&ecsdomain.TaskRecord{TaskID: "s1", LastStatus: ecsdomain.TaskStatusStopped, ContainerInstanceID: "i-1"},
		&ecsdomain.TaskRecord{TaskID: "s2", LastStatus: ecsdomain.TaskStatusStopped, ContainerInstanceID: "i-1"},
		&ecsdomain.TaskRecord{TaskID: "r2", LastStatus: ecsdomain.TaskStatusRunning, ContainerInstanceID: "i-2"},
	)

	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, ecsdomain.TestAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].RunningTasksCount))
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].PendingTasksCount))
}

// A stale reservation is exactly what made the count wrong: PlacedTasks still
// names a task that stopped, so a count taken from it reports a task nobody is
// running.
func TestDescribeContainerInstances_StalePlacedTasksDoNotInflateCount(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	kv, err := svc.Bucket(t.Context(), ecsdomain.TestAccountID)
	require.NoError(t, err)
	_, err = svc.UpsertInstance(t.Context(), kv, ecsdomain.TestAccountID, "web", "i-1", func(r *ecsdomain.InstanceRecord) {
		r.PlacedTasks = []string{"gone-1", "gone-2", "r1"}
	})
	require.NoError(t, err)

	seedTasks(t, svc, "web",
		&ecsdomain.TaskRecord{TaskID: "r1", LastStatus: ecsdomain.TaskStatusRunning, ContainerInstanceID: "i-1"},
	)

	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, ecsdomain.TestAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].RunningTasksCount))
}
