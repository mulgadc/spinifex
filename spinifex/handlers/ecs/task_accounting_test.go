package handlers_ecs_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTasks writes task records straight to KV. The statuses under test are
// reported by the agent, so driving them through RunTask would require a live
// agent to reach anything other than PENDING.
func seedTasks(t *testing.T, svc *handlers_ecs.Service, cluster string, recs ...*handlers_ecs.TaskRecord) {
	t.Helper()
	kv, err := svc.Bucket(t.Context(), handlers_ecs.TestAccountID)
	require.NoError(t, err)
	for _, r := range recs {
		r.Cluster = cluster
		r.ARN = handlers_ecs.TaskARN(svc.Region(), handlers_ecs.TestAccountID, cluster, r.TaskID)
		require.NoError(t, handlers_ecs.PutJSON(t.Context(), kv, handlers_ecs.TaskKey(cluster, r.TaskID), r))
	}
}

func listTaskIDs(t *testing.T, svc *handlers_ecs.Service, input *ecs.ListTasksInput) []string {
	t.Helper()
	out, err := svc.ListTasks(t.Context(), input, handlers_ecs.TestAccountID)
	require.NoError(t, err)
	ids := make([]string, 0, len(out.TaskArns))
	for _, a := range out.TaskArns {
		ids = append(ids, handlers_ecs.ContainerInstanceShortID(aws.StringValue(a)))
	}
	return ids
}

func TestListTasks_DesiredStatusFilters(t *testing.T) {
	svc, _ := handlers_ecs.NewTestService(t)
	seedTasks(t, svc, "web",
		&handlers_ecs.TaskRecord{TaskID: "run-1", DesiredStatus: handlers_ecs.TaskStatusRunning, LastStatus: handlers_ecs.TaskStatusRunning},
		&handlers_ecs.TaskRecord{TaskID: "stop-1", DesiredStatus: handlers_ecs.TaskStatusStopped, LastStatus: handlers_ecs.TaskStatusStopped},
		&handlers_ecs.TaskRecord{TaskID: "stop-2", DesiredStatus: handlers_ecs.TaskStatusStopped, LastStatus: handlers_ecs.TaskStatusStopped},
	)

	assert.ElementsMatch(t, []string{"run-1"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web"), DesiredStatus: aws.String(handlers_ecs.TaskStatusRunning)}))
	assert.ElementsMatch(t, []string{"stop-1", "stop-2"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web"), DesiredStatus: aws.String(handlers_ecs.TaskStatusStopped)}))
}

// The default is the case that broke a caller: asking for nothing used to hand
// back every task the cluster had ever run.
func TestListTasks_OmittedDesiredStatusMeansRunning(t *testing.T) {
	svc, _ := handlers_ecs.NewTestService(t)
	seedTasks(t, svc, "web",
		&handlers_ecs.TaskRecord{TaskID: "run-1", DesiredStatus: handlers_ecs.TaskStatusRunning, LastStatus: handlers_ecs.TaskStatusRunning},
		&handlers_ecs.TaskRecord{TaskID: "stop-1", DesiredStatus: handlers_ecs.TaskStatusStopped, LastStatus: handlers_ecs.TaskStatusStopped},
	)

	assert.ElementsMatch(t, []string{"run-1"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web")}))
}

func TestListTasks_FamilyStartedByServiceAndInstanceFilter(t *testing.T) {
	svc, _ := handlers_ecs.NewTestService(t)
	seedTasks(t, svc, "web",
		&handlers_ecs.TaskRecord{
			TaskID: "a", DesiredStatus: handlers_ecs.TaskStatusRunning, LastStatus: handlers_ecs.TaskStatusRunning,
			TaskDefFamily: "api", StartedBy: "ci", Group: handlers_ecs.ServiceTaskGroup("web"), ContainerInstanceID: "i-1",
		},
		&handlers_ecs.TaskRecord{
			TaskID: "b", DesiredStatus: handlers_ecs.TaskStatusRunning, LastStatus: handlers_ecs.TaskStatusRunning,
			TaskDefFamily: "worker", StartedBy: "human", Group: handlers_ecs.ServiceTaskGroup("batch"), ContainerInstanceID: "i-2",
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
	svc, _ := handlers_ecs.NewTestService(t)
	seedTasks(t, svc, "web",
		&handlers_ecs.TaskRecord{
			TaskID: "a", DesiredStatus: handlers_ecs.TaskStatusRunning, LastStatus: handlers_ecs.TaskStatusRunning,
			TaskDefFamily: "api", ContainerInstanceID: "i-1",
		},
		&handlers_ecs.TaskRecord{
			TaskID: "b", DesiredStatus: handlers_ecs.TaskStatusRunning, LastStatus: handlers_ecs.TaskStatusRunning,
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
	svc, _ := handlers_ecs.NewTestService(t)
	seedTasks(t, svc, "web",
		&handlers_ecs.TaskRecord{TaskID: "t-1", DesiredStatus: handlers_ecs.TaskStatusRunning, LastStatus: handlers_ecs.TaskStatusRunning},
	)

	require.NoError(t, svc.RecordTaskState(t.Context(), &bus.TaskState{
		AccountID: handlers_ecs.TestAccountID, ClusterName: "web", TaskID: "t-1",
		LastStatus: handlers_ecs.TaskStatusStopped, Reason: "container exited (1)",
	}))

	kv, err := svc.Bucket(t.Context(), handlers_ecs.TestAccountID)
	require.NoError(t, err)
	var rec handlers_ecs.TaskRecord
	found, err := handlers_ecs.GetJSON(t.Context(), kv, handlers_ecs.TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, handlers_ecs.TaskStatusStopped, rec.DesiredStatus)

	assert.Empty(t, listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web")}))
	assert.ElementsMatch(t, []string{"t-1"}, listTaskIDs(t, svc, &ecs.ListTasksInput{
		Cluster: aws.String("web"), DesiredStatus: aws.String(handlers_ecs.TaskStatusStopped),
	}))
}

func TestDescribeContainerInstances_CountsComeFromTaskRecords(t *testing.T) {
	svc, _ := handlers_ecs.NewTestService(t)
	_, err := svc.RegisterContainerInstance(t.Context(), &ecs.RegisterContainerInstanceInput{
		Cluster:                  aws.String("web"),
		InstanceIdentityDocument: aws.String("i-1"),
	}, handlers_ecs.TestAccountID)
	require.NoError(t, err)

	seedTasks(t, svc, "web",
		&handlers_ecs.TaskRecord{TaskID: "r1", LastStatus: handlers_ecs.TaskStatusRunning, ContainerInstanceID: "i-1"},
		&handlers_ecs.TaskRecord{TaskID: "p1", LastStatus: handlers_ecs.TaskStatusPending, ContainerInstanceID: "i-1"},
		&handlers_ecs.TaskRecord{TaskID: "s1", LastStatus: handlers_ecs.TaskStatusStopped, ContainerInstanceID: "i-1"},
		&handlers_ecs.TaskRecord{TaskID: "s2", LastStatus: handlers_ecs.TaskStatusStopped, ContainerInstanceID: "i-1"},
		&handlers_ecs.TaskRecord{TaskID: "r2", LastStatus: handlers_ecs.TaskStatusRunning, ContainerInstanceID: "i-2"},
	)

	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, handlers_ecs.TestAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].RunningTasksCount))
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].PendingTasksCount))
}

// A stale reservation is exactly what made the count wrong: PlacedTasks still
// names a task that stopped, so a count taken from it reports a task nobody is
// running.
func TestDescribeContainerInstances_StalePlacedTasksDoNotInflateCount(t *testing.T) {
	svc, _ := handlers_ecs.NewTestService(t)
	kv, err := svc.Bucket(t.Context(), handlers_ecs.TestAccountID)
	require.NoError(t, err)
	_, err = svc.UpsertInstance(t.Context(), kv, handlers_ecs.TestAccountID, "web", "i-1", func(r *handlers_ecs.InstanceRecord) {
		r.PlacedTasks = []string{"gone-1", "gone-2", "r1"}
	})
	require.NoError(t, err)

	seedTasks(t, svc, "web",
		&handlers_ecs.TaskRecord{TaskID: "r1", LastStatus: handlers_ecs.TaskStatusRunning, ContainerInstanceID: "i-1"},
	)

	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, handlers_ecs.TestAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].RunningTasksCount))
}
