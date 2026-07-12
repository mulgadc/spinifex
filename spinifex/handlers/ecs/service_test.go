package handlers_ecs

import (
	"context"
	"strconv"
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
	out, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ACTIVE", aws.StringValue(out.Cluster.Status))
	assert.Equal(t, ClusterARN(testRegion, testAccountID, "web"), aws.StringValue(out.Cluster.ClusterArn))

	// Recreate returns the same record, not a duplicate.
	_, err = svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	list, err := svc.ListClusters(context.Background(), &ecs.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, list.ClusterArns, 1)
}

func TestService_DescribeClusters_DefaultAndMissing(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{}, testAccountID) // implicit "default"
	require.NoError(t, err)

	out, err := svc.DescribeClusters(context.Background(), &ecs.DescribeClustersInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Clusters, 1)
	assert.Equal(t, defaultCluster, aws.StringValue(out.Clusters[0].ClusterName))

	miss, err := svc.DescribeClusters(context.Background(), &ecs.DescribeClustersInput{Clusters: []*string{aws.String("nope")}}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, miss.Clusters)
}

func TestService_DescribeClusters_ReturnsTags(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{
		ClusterName: aws.String("web"),
		Tags: []*ecs.Tag{
			{Key: aws.String("team"), Value: aws.String("infra")},
			{Key: aws.String("env"), Value: aws.String("prod")},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeClusters(context.Background(), &ecs.DescribeClustersInput{
		Clusters: []*string{aws.String("web")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Clusters, 1)

	tags := out.Clusters[0].Tags
	require.Len(t, tags, 2)
	// Stable key order: sorted ascending.
	assert.Equal(t, "env", aws.StringValue(tags[0].Key))
	assert.Equal(t, "prod", aws.StringValue(tags[0].Value))
	assert.Equal(t, "team", aws.StringValue(tags[1].Key))
	assert.Equal(t, "infra", aws.StringValue(tags[1].Value))
}

func registerTaskDef(t *testing.T, svc *Service, family string, cpu, mem int) *ecs.RegisterTaskDefinitionOutput {
	t.Helper()
	out, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
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
	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("nginx")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), aws.Int64Value(d.TaskDefinition.Revision))

	// family:rev resolves the pinned revision.
	d1, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("nginx:1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), aws.Int64Value(d1.TaskDefinition.Revision))

	list, err := svc.ListTaskDefinitions(context.Background(), &ecs.ListTaskDefinitionsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, list.TaskDefinitionArns, 2)
}

func TestService_ListTaskDefinitions_StatusFilter(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "keep", 128, 256)
	registerTaskDef(t, svc, "gone", 128, 256)

	_, err := svc.DeregisterTaskDefinition(context.Background(), &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: aws.String("gone:1"),
	}, testAccountID)
	require.NoError(t, err)

	// Default (unset) status lists ACTIVE only; the deregistered revision drops.
	active, err := svc.ListTaskDefinitions(context.Background(), &ecs.ListTaskDefinitionsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, active.TaskDefinitionArns, 1)
	assert.Contains(t, aws.StringValue(active.TaskDefinitionArns[0]), "keep:1")

	// Explicit ACTIVE matches the default.
	activeExplicit, err := svc.ListTaskDefinitions(context.Background(), &ecs.ListTaskDefinitionsInput{
		Status: aws.String(TaskDefStatusActive),
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, activeExplicit.TaskDefinitionArns, 1)

	// INACTIVE returns only the deregistered revision.
	inactive, err := svc.ListTaskDefinitions(context.Background(), &ecs.ListTaskDefinitionsInput{
		Status: aws.String(TaskDefStatusInactive),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, inactive.TaskDefinitionArns, 1)
	assert.Contains(t, aws.StringValue(inactive.TaskDefinitionArns[0]), "gone:1")
}

func TestService_RegisterTaskDefinition_NoFamily(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{}, testAccountID)
	require.Error(t, err)
}

func TestService_DescribeTaskDefinition_Unknown(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("ghost")}, testAccountID)
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
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster:        aws.String("web"),
		TaskDefinition: aws.String("app"),
		Count:          aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Empty(t, out.Failures)
	assert.Equal(t, "PENDING", aws.StringValue(out.Tasks[0].LastStatus))

	// Assign written to the instance's KV inbox, drained by polling the gateway.
	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	as := poll.Assignments[0]
	assert.Equal(t, "i-1", as.InstanceID)
	require.Len(t, as.Containers, 1)
	assert.Equal(t, "registry/app:1", as.Containers[0].Image)

	// Capacity reserved on the instance.
	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, di.ContainerInstances, 1)
	assert.Equal(t, int64(1), aws.Int64Value(di.ContainerInstances[0].RunningTasksCount))

	// Task visible via Describe/List.
	lt, err := svc.ListTasks(context.Background(), &ecs.ListTasksInput{Cluster: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, lt.TaskArns, 1)
	assert.Equal(t, as.TaskID, containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn)))
}

func TestService_RunTask_AssignCarriesTaskRole(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	roleARN := "arn:aws:iam::123456789012:role/task-app"
	_, err = svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family:      aws.String("app"),
		TaskRoleArn: aws.String(roleARN),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"),
			Cpu: aws.Int64(128), Memory: aws.Int64(256), Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	// Round-trips through DescribeTaskDefinition.
	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, roleARN, aws.StringValue(d.TaskDefinition.TaskRoleArn))

	_, err = svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)

	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	assert.Equal(t, roleARN, poll.Assignments[0].TaskRoleARN)
}

// PollAssignments is at-least-once: re-poll without ack re-delivers the assign;
// acking it (then STOPPED) drains the inbox so it is never re-delivered.
func TestService_PollAssignments_AckAndReclaim(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	taskID := containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn))

	// Unacked re-poll re-delivers.
	p1, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, p1.Assignments, 1)
	p2, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, p2.Assignments, 1)

	// Ack drains the inbox.
	p3, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{
		Cluster: "web", ContainerInstance: "i-1", AckTaskIDs: []string{taskID},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, p3.Assignments)

	// A fresh RunTask + STOPPED reclaims its inbox entry without an explicit ack.
	out2, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	task2 := containerInstanceShortID(aws.StringValue(out2.Tasks[0].TaskArn))
	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: task2,
		LastStatus: bus.TaskStatusStopped,
	}))
	p4, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, p4.Assignments)
}

func TestService_RunTask_ClusterNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 1, 1)
	_, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{Cluster: aws.String("ghost"), TaskDefinition: aws.String("app")}, testAccountID)
	require.Error(t, err)
}

func TestService_RunTask_NoCapacityFails(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "big", 100, 100000) // 100GB memory, won't fit
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("big"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)
}

// Task-state RUNNING then STOPPED updates the task and releases capacity.
func TestService_RecordTaskState_ReleasesCapacityOnStop(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	taskID := containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn))

	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
		LastStatus: bus.TaskStatusRunning,
	}))
	exit := 0
	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
		LastStatus: bus.TaskStatusStopped, Reason: "exited",
		Containers: []bus.ContainerStatus{{Name: "app", Status: bus.TaskStatusStopped, ExitCode: &exit}},
	}))

	dt, err := svc.DescribeTasks(context.Background(), &ecs.DescribeTasksInput{Cluster: aws.String("web"), Tasks: []*string{aws.String(taskID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, dt.Tasks, 1)
	assert.Equal(t, "STOPPED", aws.StringValue(dt.Tasks[0].LastStatus))

	// Capacity fully released: remaining == total.
	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
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
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	taskARN := aws.StringValue(out.Tasks[0].TaskArn) // full ARN exercises taskShortID

	exit := int64(0)
	ack, err := svc.SubmitTaskStateChange(context.Background(), &ecs.SubmitTaskStateChangeInput{
		Cluster: aws.String("web"), Task: aws.String(taskARN),
		Status: aws.String(bus.TaskStatusStopped), Reason: aws.String("exited"),
		Containers: []*ecs.ContainerStateChange{
			{ContainerName: aws.String("app"), Status: aws.String(bus.TaskStatusStopped),
				ExitCode: &exit, RuntimeId: aws.String("ctr-1")},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, ack.Acknowledgment)

	dt, err := svc.DescribeTasks(context.Background(), &ecs.DescribeTasksInput{
		Cluster: aws.String("web"), Tasks: []*string{aws.String(taskARN)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, dt.Tasks, 1)
	assert.Equal(t, "STOPPED", aws.StringValue(dt.Tasks[0].LastStatus))

	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
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
	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "DRAINING", aws.StringValue(di.ContainerInstances[0].Status))
}

// The reaper marks a stale instance DRAINING and stops its tasks.
func TestScheduler_ReapBucket_StopsStaleInstanceTasks(t *testing.T) {
	svc, nc := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
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
	sc.reapBucket(context.Background(), kv, testAccountID, time.Now().UTC())

	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "DRAINING", aws.StringValue(di.ContainerInstances[0].Status))

	dt, err := svc.DescribeTasks(context.Background(), &ecs.DescribeTasksInput{Cluster: aws.String("web"), Tasks: []*string{aws.String(taskID)}}, testAccountID)
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

// --- GPU placement dimension (Epic C2) ---

// registerTaskDefGPU registers a single-container task def requesting gpu
// whole-GPUs via resourceRequirements (AWS ECS GPU semantics).
func registerTaskDefGPU(t *testing.T, svc *Service, family string, cpu, mem, gpu int) *ecs.RegisterTaskDefinitionOutput {
	t.Helper()
	out, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String(family),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name:      aws.String("app"),
			Image:     aws.String("registry/app:1"),
			Cpu:       aws.Int64(int64(cpu)),
			Memory:    aws.Int64(int64(mem)),
			Essential: aws.Bool(true),
			ResourceRequirements: []*ecs.ResourceRequirement{
				{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String(strconv.Itoa(gpu))},
			},
		}},
	}, testAccountID)
	require.NoError(t, err)
	return out
}

// registerInstanceGPU seeds an ACTIVE container instance with CPU/memory/GPU
// capacity via the bus register path.
func registerInstanceGPU(t *testing.T, svc *Service, cluster, id string, cpu, mem, gpu int) {
	t.Helper()
	require.NoError(t, svc.recordRegister(&bus.RegisterInstance{
		AccountID:   testAccountID,
		ClusterName: cluster,
		InstanceID:  id,
		Capacity:    bus.InstanceCapacity{CPU: cpu, MemoryMiB: mem, GPU: gpu},
	}))
}

func TestService_RunTask_GPU_ReservesOnPlacement(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDefGPU(t, svc, "gpu-app", 128, 256, 1)
	registerInstanceGPU(t, svc, "web", "i-1", 1024, 2048, 2)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("gpu-app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Empty(t, out.Failures)
	taskID := containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn))

	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, di.ContainerInstances, 1)

	// Registered GPU capacity is reported; remaining reflects the 1-GPU reservation.
	var registeredGPU, remainingGPU *ecs.Resource
	for _, r := range di.ContainerInstances[0].RegisteredResources {
		if aws.StringValue(r.Name) == "GPU" {
			registeredGPU = r
		}
	}
	for _, r := range di.ContainerInstances[0].RemainingResources {
		if aws.StringValue(r.Name) == "GPU" {
			remainingGPU = r
		}
	}
	require.NotNil(t, registeredGPU)
	require.NotNil(t, remainingGPU)
	assert.Equal(t, "STRINGSET", aws.StringValue(registeredGPU.Type))
	assert.Equal(t, "STRINGSET", aws.StringValue(remainingGPU.Type))
	// No agent-reported UUIDs yet (Epic C3): both stringSets are placeholder-empty,
	// but the underlying instance record's counts prove the reservation happened.
	assert.Empty(t, registeredGPU.StringSetValue)
	assert.Empty(t, remainingGPU.StringSetValue)

	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	var rec InstanceRecord
	found, err := getJSON(kv, InstanceKey("web", "i-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 2, rec.TotalGPU)
	assert.Equal(t, 1, rec.ReservedGPU)

	// Stopping the task releases the GPU reservation.
	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
		LastStatus: bus.TaskStatusStopped, Reason: "exited",
	}))
	found, err = getJSON(kv, InstanceKey("web", "i-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 0, rec.ReservedGPU)
}

func TestService_RunTask_GPU_NoCapacityFails(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDefGPU(t, svc, "gpu-app", 128, 256, 2)
	registerInstanceGPU(t, svc, "web", "i-1", 1024, 2048, 1) // only 1 GPU available, task needs 2

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("gpu-app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)
}

func TestService_DescribeContainerInstances_NoGPUOmitsResource(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, di.ContainerInstances, 1)
	for _, r := range di.ContainerInstances[0].RegisteredResources {
		assert.NotEqual(t, "GPU", aws.StringValue(r.Name))
	}
	for _, r := range di.ContainerInstances[0].RemainingResources {
		assert.NotEqual(t, "GPU", aws.StringValue(r.Name))
	}
}

// RegisterContainerInstance (the AWS-API path) reports GPU as a STRINGSET of
// device UUIDs, matching real ECS's TotalResources shape; the count becomes the
// instance's placement capacity.
func TestRegisterContainerInstance_GPU_StringSetResource(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.RegisterContainerInstance(context.Background(), &ecs.RegisterContainerInstanceInput{
		Cluster:                  aws.String("web"),
		InstanceIdentityDocument: aws.String("i-gpu"),
		TotalResources: []*ecs.Resource{
			{Name: aws.String("CPU"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(1024)},
			{Name: aws.String("MEMORY"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(2048)},
			{Name: aws.String("GPU"), Type: aws.String("STRINGSET"), StringSetValue: aws.StringSlice([]string{"GPU-aaa", "GPU-bbb"})},
		},
	}, testAccountID)
	require.NoError(t, err)

	rec := instanceStatus(t, svc, "web", "i-gpu")
	assert.Equal(t, 2, rec.TotalGPU)
	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb"}, rec.GPUIDs)

	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-gpu")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, di.ContainerInstances, 1)
	var gpuRes *ecs.Resource
	for _, r := range di.ContainerInstances[0].RegisteredResources {
		if aws.StringValue(r.Name) == "GPU" {
			gpuRes = r
		}
	}
	require.NotNil(t, gpuRes)
	assert.Equal(t, "STRINGSET", aws.StringValue(gpuRes.Type))
	assert.ElementsMatch(t, []string{"GPU-aaa", "GPU-bbb"}, aws.StringValueSlice(gpuRes.StringSetValue))
}

// The Layer-2 bus register path (recordRegister) also carries the agent's
// discovered device UUIDs, mirroring the AWS-API RegisterContainerInstance
// path (Epic C3 register report-back).
func TestRecordRegister_GPU_CarriesDeviceUUIDs(t *testing.T) {
	svc, _ := newTestService(t)
	require.NoError(t, svc.recordRegister(&bus.RegisterInstance{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1",
		Capacity: bus.InstanceCapacity{CPU: 1024, MemoryMiB: 2048, GPU: 2, GPUIDs: []string{"GPU-aaa", "GPU-bbb"}},
	}))

	rec := instanceStatus(t, svc, "web", "i-1")
	assert.Equal(t, 2, rec.TotalGPU)
	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb"}, rec.GPUIDs)
}

func TestAccountIDFromBucket(t *testing.T) {
	id, ok := accountIDFromBucket(AccountBucketName(testAccountID))
	assert.True(t, ok)
	assert.Equal(t, testAccountID, id)
	_, ok = accountIDFromBucket("some-other-bucket")
	assert.False(t, ok)
}
