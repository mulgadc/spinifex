package handlers_ecs

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTasks writes task records straight to KV. The statuses under test are
// reported by the agent, so driving them through RunTask would require a live
// agent to reach anything other than PENDING.
func seedTasks(t *testing.T, svc *Service, cluster string, recs ...*TaskRecord) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	for _, r := range recs {
		r.Cluster = cluster
		r.ARN = TaskARN(svc.region, testAccountID, cluster, r.TaskID)
		require.NoError(t, putJSON(t.Context(), kv, TaskKey(cluster, r.TaskID), r))
	}
}

func listTaskIDs(t *testing.T, svc *Service, input *ecs.ListTasksInput) []string {
	t.Helper()
	out, err := svc.ListTasks(t.Context(), input, testAccountID)
	require.NoError(t, err)
	ids := make([]string, 0, len(out.TaskArns))
	for _, a := range out.TaskArns {
		ids = append(ids, ContainerInstanceShortID(aws.StringValue(a)))
	}
	return ids
}

func TestListTasks_DesiredStatusFilters(t *testing.T) {
	svc, _ := newTestService(t)
	seedTasks(t, svc, "web",
		&TaskRecord{TaskID: "run-1", DesiredStatus: TaskStatusRunning, LastStatus: TaskStatusRunning},
		&TaskRecord{TaskID: "stop-1", DesiredStatus: TaskStatusStopped, LastStatus: TaskStatusStopped},
		&TaskRecord{TaskID: "stop-2", DesiredStatus: TaskStatusStopped, LastStatus: TaskStatusStopped},
	)

	assert.ElementsMatch(t, []string{"run-1"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web"), DesiredStatus: aws.String(TaskStatusRunning)}))
	assert.ElementsMatch(t, []string{"stop-1", "stop-2"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web"), DesiredStatus: aws.String(TaskStatusStopped)}))
}

// The default is the case that broke a caller: asking for nothing used to hand
// back every task the cluster had ever run.
func TestListTasks_OmittedDesiredStatusMeansRunning(t *testing.T) {
	svc, _ := newTestService(t)
	seedTasks(t, svc, "web",
		&TaskRecord{TaskID: "run-1", DesiredStatus: TaskStatusRunning, LastStatus: TaskStatusRunning},
		&TaskRecord{TaskID: "stop-1", DesiredStatus: TaskStatusStopped, LastStatus: TaskStatusStopped},
	)

	assert.ElementsMatch(t, []string{"run-1"},
		listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web")}))
}

func TestListTasks_FamilyStartedByServiceAndInstanceFilter(t *testing.T) {
	svc, _ := newTestService(t)
	seedTasks(t, svc, "web",
		&TaskRecord{
			TaskID: "a", DesiredStatus: TaskStatusRunning, LastStatus: TaskStatusRunning,
			TaskDefFamily: "api", StartedBy: "ci", Group: serviceTaskGroup("web"), ContainerInstanceID: "i-1",
		},
		&TaskRecord{
			TaskID: "b", DesiredStatus: TaskStatusRunning, LastStatus: TaskStatusRunning,
			TaskDefFamily: "worker", StartedBy: "human", Group: serviceTaskGroup("batch"), ContainerInstanceID: "i-2",
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
	svc, _ := newTestService(t)
	seedTasks(t, svc, "web",
		&TaskRecord{
			TaskID: "a", DesiredStatus: TaskStatusRunning, LastStatus: TaskStatusRunning,
			TaskDefFamily: "api", ContainerInstanceID: "i-1",
		},
		&TaskRecord{
			TaskID: "b", DesiredStatus: TaskStatusRunning, LastStatus: TaskStatusRunning,
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
	svc, _ := newTestService(t)
	seedTasks(t, svc, "web",
		&TaskRecord{TaskID: "t-1", DesiredStatus: TaskStatusRunning, LastStatus: TaskStatusRunning},
	)

	require.NoError(t, svc.recordTaskState(t.Context(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", TaskID: "t-1",
		LastStatus: TaskStatusStopped, Reason: "container exited (1)",
	}))

	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec TaskRecord
	found, err := getJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, TaskStatusStopped, rec.DesiredStatus)

	assert.Empty(t, listTaskIDs(t, svc, &ecs.ListTasksInput{Cluster: aws.String("web")}))
	assert.ElementsMatch(t, []string{"t-1"}, listTaskIDs(t, svc, &ecs.ListTasksInput{
		Cluster: aws.String("web"), DesiredStatus: aws.String(TaskStatusStopped),
	}))
}

func TestDescribeContainerInstances_CountsComeFromTaskRecords(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterContainerInstance(t.Context(), &ecs.RegisterContainerInstanceInput{
		Cluster:                  aws.String("web"),
		InstanceIdentityDocument: aws.String("i-1"),
	}, testAccountID)
	require.NoError(t, err)

	seedTasks(t, svc, "web",
		&TaskRecord{TaskID: "r1", LastStatus: TaskStatusRunning, ContainerInstanceID: "i-1"},
		&TaskRecord{TaskID: "p1", LastStatus: TaskStatusPending, ContainerInstanceID: "i-1"},
		&TaskRecord{TaskID: "s1", LastStatus: TaskStatusStopped, ContainerInstanceID: "i-1"},
		&TaskRecord{TaskID: "s2", LastStatus: TaskStatusStopped, ContainerInstanceID: "i-1"},
		&TaskRecord{TaskID: "r2", LastStatus: TaskStatusRunning, ContainerInstanceID: "i-2"},
	)

	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].RunningTasksCount))
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].PendingTasksCount))
}

// A stale reservation is exactly what made the count wrong: PlacedTasks still
// names a task that stopped, so a count taken from it reports a task nobody is
// running.
func TestDescribeContainerInstances_StalePlacedTasksDoNotInflateCount(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	_, err = svc.upsertInstance(t.Context(), kv, testAccountID, "web", "i-1", func(r *InstanceRecord) {
		r.PlacedTasks = []string{"gone-1", "gone-2", "r1"}
	})
	require.NoError(t, err)

	seedTasks(t, svc, "web",
		&TaskRecord{TaskID: "r1", LastStatus: TaskStatusRunning, ContainerInstanceID: "i-1"},
	)

	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(out.ContainerInstances[0].RunningTasksCount))
}
