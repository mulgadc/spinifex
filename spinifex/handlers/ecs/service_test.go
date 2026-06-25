package handlers_ecs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRegion = "ap-southeast-2"

func newTestService(t *testing.T) (*Service, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return NewService(nc, testRegion, "internal"), nc
}

func TestService_CreateCluster_Idempotent(t *testing.T) {
	svc, _ := newTestService(t)
	out, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ACTIVE", aws.StringValue(out.Cluster.Status))
	assert.Equal(t, ClusterARN(testRegion, testAccountID, "web"), aws.StringValue(out.Cluster.ClusterArn))

	// Recreate returns the same record, not a duplicate.
	_, err = svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	list, err := svc.ListClusters(&ecs.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, list.ClusterArns, 1)
}

func TestService_DescribeClusters_DefaultAndMissing(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{}, testAccountID) // implicit "default"
	require.NoError(t, err)

	out, err := svc.DescribeClusters(&ecs.DescribeClustersInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Clusters, 1)
	assert.Equal(t, defaultCluster, aws.StringValue(out.Clusters[0].ClusterName))

	miss, err := svc.DescribeClusters(&ecs.DescribeClustersInput{Clusters: []*string{aws.String("nope")}}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, miss.Clusters)
}

func registerTaskDef(t *testing.T, svc *Service, family string, cpu, mem int) *ecs.RegisterTaskDefinitionOutput {
	t.Helper()
	out, err := svc.RegisterTaskDefinition(&ecs.RegisterTaskDefinitionInput{
		Family: aws.String(family),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name:      aws.String("app"),
			Image:     aws.String("registry/app:1"),
			Cpu:       aws.Int64(int64(cpu)),
			Memory:    aws.Int64(int64(mem)),
			Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)
	return out
}

func TestService_RegisterTaskDefinition_RevisionBump(t *testing.T) {
	svc, _ := newTestService(t)
	r1 := registerTaskDef(t, svc, "nginx", 128, 256)
	assert.Equal(t, int64(1), aws.Int64Value(r1.TaskDefinition.Revision))
	r2 := registerTaskDef(t, svc, "nginx", 128, 256)
	assert.Equal(t, int64(2), aws.Int64Value(r2.TaskDefinition.Revision))

	// Bare family resolves to latest.
	d, err := svc.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("nginx")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), aws.Int64Value(d.TaskDefinition.Revision))

	// family:rev resolves the pinned revision.
	d1, err := svc.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("nginx:1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), aws.Int64Value(d1.TaskDefinition.Revision))

	list, err := svc.ListTaskDefinitions(&ecs.ListTaskDefinitionsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, list.TaskDefinitionArns, 2)
}

func TestService_RegisterTaskDefinition_NoFamily(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(&ecs.RegisterTaskDefinitionInput{}, testAccountID)
	require.Error(t, err)
}

func TestService_DescribeTaskDefinition_Unknown(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("ghost")}, testAccountID)
	require.Error(t, err)
}

// registerInstance seeds an ACTIVE container instance with capacity via the bus
// register path (the way the agent registers in production).
func registerInstance(t *testing.T, svc *Service, cluster, id string, cpu, mem int) {
	t.Helper()
	require.NoError(t, svc.recordRegister(&bus.RegisterInstance{
		AccountID:   testAccountID,
		ClusterName: cluster,
		InstanceID:  id,
		Capacity:    bus.InstanceCapacity{CPU: cpu, MemoryMiB: mem},
	}))
}

func TestService_RunTask_PlacesAndAssigns(t *testing.T) {
	svc, nc := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	sub, err := nc.SubscribeSync(bus.AssignSubject(testAccountID, "web", "i-1"))
	require.NoError(t, err)

	out, err := svc.RunTask(&ecs.RunTaskInput{
		Cluster:        aws.String("web"),
		TaskDefinition: aws.String("app"),
		Count:          aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Empty(t, out.Failures)
	assert.Equal(t, "PENDING", aws.StringValue(out.Tasks[0].LastStatus))

	// Assign published to the instance's agent.
	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var as bus.Assign
	require.NoError(t, json.Unmarshal(msg.Data, &as))
	assert.Equal(t, "i-1", as.InstanceID)
	require.Len(t, as.Containers, 1)
	assert.Equal(t, "registry/app:1", as.Containers[0].Image)

	// Capacity reserved on the instance.
	di, err := svc.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, di.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(di.ContainerInstances[0].RunningTasksCount))

	// Task visible via Describe/List.
	lt, err := svc.ListTasks(&ecs.ListTasksInput{Cluster: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, lt.TaskArns, 1)
	assert.Equal(t, as.TaskID, containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn)))
}

func TestService_RunTask_ClusterNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 1, 1)
	_, err := svc.RunTask(&ecs.RunTaskInput{Cluster: aws.String("ghost"), TaskDefinition: aws.String("app")}, testAccountID)
	require.Error(t, err)
}

func TestService_RunTask_NoCapacityFails(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "big", 100, 100000) // 100GB memory, won't fit
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(&ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("big"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)
}

// Task-state RUNNING then STOPPED updates the task and releases capacity.
func TestService_RecordTaskState_ReleasesCapacityOnStop(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(&ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	taskID := containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn))

	require.NoError(t, svc.recordTaskState(&bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
		LastStatus: bus.TaskStatusRunning,
	}))
	exit := 0
	require.NoError(t, svc.recordTaskState(&bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
		LastStatus: bus.TaskStatusStopped, Reason: "exited",
		Containers: []bus.ContainerStatus{{Name: "app", Status: bus.TaskStatusStopped, ExitCode: &exit}},
	}))

	dt, err := svc.DescribeTasks(&ecs.DescribeTasksInput{Cluster: aws.String("web"), Tasks: []*string{aws.String(taskID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, dt.Tasks, 1)
	assert.Equal(t, "STOPPED", aws.StringValue(dt.Tasks[0].LastStatus))

	// Capacity fully released: remaining == total.
	di, err := svc.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), aws.Int64Value(di.ContainerInstances[0].RunningTasksCount))
	for _, r := range di.ContainerInstances[0].RemainingResources {
		switch aws.StringValue(r.Name) {
		case "CPU":
			assert.Equal(t, int64(1024), aws.Int64Value(r.IntegerValue))
		case "MEMORY":
			assert.Equal(t, int64(2048), aws.Int64Value(r.IntegerValue))
		}
	}
}

// The AWS-API SubmitTaskStateChange path (gateway-routed agent) converges on the
// same task record + capacity release as the bus path, and resolves a task ARN.
func TestService_SubmitTaskStateChange_StopsTask(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(&ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	taskARN := aws.StringValue(out.Tasks[0].TaskArn) // full ARN exercises taskShortID

	exit := int64(0)
	ack, err := svc.SubmitTaskStateChange(&ecs.SubmitTaskStateChangeInput{
		Cluster: aws.String("web"), Task: aws.String(taskARN),
		Status: aws.String(bus.TaskStatusStopped), Reason: aws.String("exited"),
		Containers: []*ecs.ContainerStateChange{
			{ContainerName: aws.String("app"), Status: aws.String(bus.TaskStatusStopped),
				ExitCode: &exit, RuntimeId: aws.String("ctr-1")},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, ack.Acknowledgment)

	dt, err := svc.DescribeTasks(&ecs.DescribeTasksInput{
		Cluster: aws.String("web"), Tasks: []*string{aws.String(taskARN)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, dt.Tasks, 1)
	assert.Equal(t, "STOPPED", aws.StringValue(dt.Tasks[0].LastStatus))

	di, err := svc.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), aws.Int64Value(di.ContainerInstances[0].RunningTasksCount))
}

func TestService_RecordHeartbeat_UpdatesStatus(t *testing.T) {
	svc, _ := newTestService(t)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	require.NoError(t, svc.recordHeartbeat(&bus.Heartbeat{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", Status: bus.StatusDraining,
	}))
	di, err := svc.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "DRAINING", aws.StringValue(di.ContainerInstances[0].Status))
}

// The reaper marks a stale instance DRAINING and stops its tasks.
func TestScheduler_ReapBucket_StopsStaleInstanceTasks(t *testing.T) {
	svc, nc := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(&ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	taskID := containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn))

	// Backdate LastSeen beyond the heartbeat timeout.
	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	var rec InstanceRecord
	_, err = getJSON(kv, InstanceKey("web", "i-1"), &rec)
	require.NoError(t, err)
	rec.LastSeen = time.Now().UTC().Add(-2 * heartbeatTimeout)
	require.NoError(t, putJSON(kv, InstanceKey("web", "i-1"), &rec))

	sc := NewScheduler(nc, svc, "test-holder")
	sc.reapBucket(kv, testAccountID, time.Now().UTC())

	di, err := svc.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "DRAINING", aws.StringValue(di.ContainerInstances[0].Status))

	dt, err := svc.DescribeTasks(&ecs.DescribeTasksInput{Cluster: aws.String("web"), Tasks: []*string{aws.String(taskID)}}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "STOPPED", aws.StringValue(dt.Tasks[0].LastStatus))
	assert.Equal(t, stoppedReasonReaped, aws.StringValue(dt.Tasks[0].StoppedReason))
}

// Leader election: first holder wins Create; a second holder is rejected.
func TestScheduler_AcquireLease_SingleLeader(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := NewService(nc, testRegion, "")
	a := NewScheduler(nc, svc, "holder-a")
	b := NewScheduler(nc, svc, "holder-b")
	assert.True(t, a.acquireOrRefresh())
	assert.False(t, b.acquireOrRefresh())
	assert.True(t, a.acquireOrRefresh()) // refresh keeps leadership
}

func TestAccountIDFromBucket(t *testing.T) {
	id, ok := accountIDFromBucket(AccountBucketName(testAccountID))
	assert.True(t, ok)
	assert.Equal(t, testAccountID, id)
	_, ok = accountIDFromBucket("some-other-bucket")
	assert.False(t, ok)
}
