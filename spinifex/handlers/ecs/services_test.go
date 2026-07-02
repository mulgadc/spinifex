package handlers_ecs

import (
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRegistrar records ELBv2 target calls so the RUNNING/STOPPED hooks can be
// asserted without a live elbv2 daemon.
type stubRegistrar struct {
	registered   []string
	deregistered []string
}

func key(tg, ip string, port int) string { return fmt.Sprintf("%s|%s|%d", tg, ip, port) }

func (r *stubRegistrar) Register(_, tg, ip string, port int) error {
	r.registered = append(r.registered, key(tg, ip, port))
	return nil
}

func (r *stubRegistrar) Deregister(_, tg, ip string, port int) error {
	r.deregistered = append(r.deregistered, key(tg, ip, port))
	return nil
}

// serviceTestRig wires a Service with a stub registrar and a seeded cluster +
// task definition + one fat container instance.
func serviceTestRig(t *testing.T) (*Service, *stubRegistrar, nats.KeyValue) {
	t.Helper()
	svc, _ := newTestService(t)
	reg := &stubRegistrar{}
	svc.targets = reg
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 4096, 8192)
	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	return svc, reg, kv
}

func TestService_CreateService_LaunchesReplicas(t *testing.T) {
	svc, _, kv := serviceTestRig(t)

	out, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster:        aws.String("web"),
		ServiceName:    aws.String("web"),
		TaskDefinition: aws.String("app"),
		DesiredCount:   aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, ServiceStatusActive, aws.StringValue(out.Service.Status))
	assert.Equal(t, ServiceARN(testRegion, testAccountID, "web", "web"), aws.StringValue(out.Service.ServiceArn))
	assert.Equal(t, int64(2), aws.Int64Value(out.Service.PendingCount))

	tasks, err := svc.listServiceTasks(kv, "web", "web")
	require.NoError(t, err)
	require.Len(t, tasks, 2)
	for _, task := range tasks {
		assert.Equal(t, serviceTaskGroup("web"), task.Group)
		assert.Equal(t, TaskStatusPending, task.LastStatus)
	}
}

func TestService_CreateService_RejectsDaemonAndRegistries(t *testing.T) {
	svc, _, _ := serviceTestRig(t)

	_, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("d"), TaskDefinition: aws.String("app"),
		SchedulingStrategy: aws.String(SchedulingStrategyDaemon),
	}, testAccountID)
	require.Error(t, err)

	_, err = svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("r"), TaskDefinition: aws.String("app"),
		ServiceRegistries: []*ecs.ServiceRegistry{{RegistryArn: aws.String("arn:aws:servicediscovery:::service/x")}},
	}, testAccountID)
	require.Error(t, err)
}

func TestService_CreateService_UnknownCluster(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 1, 1)
	_, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("ghost"), ServiceName: aws.String("s"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(1),
	}, testAccountID)
	require.Error(t, err)
}

// Reconcile relaunches the desired count after a task STOPs.
func TestService_Reconcile_ReplacesStoppedTask(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)

	tasks, err := svc.listServiceTasks(kv, "web", "web")
	require.NoError(t, err)
	require.Len(t, tasks, 2)

	// One task stops (agent report).
	require.NoError(t, svc.recordTaskState(&bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1",
		TaskID: tasks[0].TaskID, LastStatus: bus.TaskStatusStopped,
	}))
	live, err := svc.listServiceTasks(kv, "web", "web")
	require.NoError(t, err)
	require.Len(t, live, 1) // STOPPED task drops out of the active set

	// Reconcile launches a replacement back to desired=2.
	svc.reconcileAllServices()
	live, err = svc.listServiceTasks(kv, "web", "web")
	require.NoError(t, err)
	assert.Len(t, live, 2)
}

// UpdateService desiredCount=1 stops the surplus task.
func TestService_UpdateService_ScalesIn(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(3),
	}, testAccountID)
	require.NoError(t, err)
	live, _ := svc.listServiceTasks(kv, "web", "web")
	require.Len(t, live, 3)

	out, err := svc.UpdateService(&ecs.UpdateServiceInput{
		Cluster: aws.String("web"), Service: aws.String("web"), DesiredCount: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), aws.Int64Value(out.Service.DesiredCount))

	live, err = svc.listServiceTasks(kv, "web", "web")
	require.NoError(t, err)
	assert.Len(t, live, 1)
}

func TestService_DeleteService_DrainsAndInactivates(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	_, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DeleteService(&ecs.DeleteServiceInput{
		Cluster: aws.String("web"), Service: aws.String("web"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, ServiceStatusInactive, aws.StringValue(out.Service.Status))

	live, err := svc.listServiceTasks(kv, "web", "web")
	require.NoError(t, err)
	assert.Empty(t, live)

	// INACTIVE service still describes; reconcile leaves it alone.
	desc, err := svc.DescribeServices(&ecs.DescribeServicesInput{
		Cluster: aws.String("web"), Services: []*string{aws.String("web")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Services, 1)
	svc.reconcileAllServices()
	live, _ = svc.listServiceTasks(kv, "web", "web")
	assert.Empty(t, live)
}

func TestService_DescribeAndList(t *testing.T) {
	svc, _, _ := serviceTestRig(t)
	for _, name := range []string{"a", "b"} {
		_, err := svc.CreateService(&ecs.CreateServiceInput{
			Cluster: aws.String("web"), ServiceName: aws.String(name), TaskDefinition: aws.String("app"),
			DesiredCount: aws.Int64(0),
		}, testAccountID)
		require.NoError(t, err)
	}
	list, err := svc.ListServices(&ecs.ListServicesInput{Cluster: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, list.ServiceArns, 2)

	miss, err := svc.DescribeServices(&ecs.DescribeServicesInput{
		Cluster: aws.String("web"), Services: []*string{aws.String("nope")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, miss.Services)
	assert.Len(t, miss.Failures, 1)
}

// StartTask places one task per named container instance and assigns it.
func TestService_StartTask_PlacesPerInstance(t *testing.T) {
	svc, _, kv := serviceTestRig(t)

	out, err := svc.StartTask(&ecs.StartTaskInput{
		Cluster:            aws.String("web"),
		TaskDefinition:     aws.String("app"),
		ContainerInstances: []*string{aws.String("i-1")},
		Group:              aws.String("custom"),
		StartedBy:          aws.String("operator"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Empty(t, out.Failures)
	assert.Equal(t, TaskStatusPending, aws.StringValue(out.Tasks[0].LastStatus))

	tasks, err := svc.listTaskRecords(kv, "web")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "custom", tasks[0].Group)
	assert.Equal(t, "operator", tasks[0].StartedBy)
	assert.Equal(t, "i-1", tasks[0].ContainerInstanceID)
}

// StartTask onto an unknown / too-small instance returns a placement Failure,
// not a hard error, and onto an unknown cluster a ClusterNotFound error.
func TestService_StartTask_Failures(t *testing.T) {
	svc, _, _ := serviceTestRig(t)

	out, err := svc.StartTask(&ecs.StartTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"),
		ContainerInstances: []*string{aws.String("i-absent")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)

	_, err = svc.StartTask(&ecs.StartTaskInput{
		Cluster: aws.String("ghost"), TaskDefinition: aws.String("app"),
		ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.Error(t, err)
}

// StopTask transitions a running service task to STOPPED, releases its
// reservation, and deregisters its ELBv2 targets.
func TestService_StopTask_StopsAndDeregisters(t *testing.T) {
	svc, reg, kv := serviceTestRig(t)
	const tgARN = "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:targetgroup/web/abc"

	_, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(0),
		LoadBalancers: []*ecs.LoadBalancer{{
			TargetGroupArn: aws.String(tgARN), ContainerName: aws.String("app"), ContainerPort: aws.Int64(80),
		}},
	}, testAccountID)
	require.NoError(t, err)

	task := &TaskRecord{
		TaskID: "t-9", Cluster: "web", Group: serviceTaskGroup("web"),
		ContainerInstanceID: "i-1", LastStatus: TaskStatusRunning, DesiredStatus: TaskStatusRunning,
		NetworkMode: NetworkModeAwsvpc, ENIPrivateIP: "10.0.1.9",
		ReservedCPU: 64, ReservedMemoryMiB: 64,
	}
	require.NoError(t, putJSON(kv, TaskKey("web", "t-9"), task))

	// Cooperative stop: the task stays RUNNING with desiredStatus=STOPPED and a
	// stop directive lands in the instance inbox; nothing is deregistered until the
	// agent reaps the container and reports STOPPED.
	out, err := svc.StopTask(&ecs.StopTaskInput{
		Cluster: aws.String("web"), Task: aws.String("t-9"), Reason: aws.String("bye"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatusRunning, aws.StringValue(out.Task.LastStatus))
	assert.Equal(t, TaskStatusStopped, aws.StringValue(out.Task.DesiredStatus))
	assert.Empty(t, reg.deregistered)

	var sd bus.StopDirective
	found, err := getJSON(kv, StopKey("web", "i-1", "t-9"), &sd)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "bye", sd.Reason)

	// Agent reports STOPPED → deregister + stop-inbox reclaimed.
	require.NoError(t, svc.recordTaskState(&bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1",
		TaskID: "t-9", LastStatus: bus.TaskStatusStopped,
	}))
	assert.Equal(t, []string{key(tgARN, "10.0.1.9", 80)}, reg.deregistered)
	found, err = getJSON(kv, StopKey("web", "i-1", "t-9"), &sd)
	require.NoError(t, err)
	assert.False(t, found)

	// Idempotent: a second stop is a no-op (already STOPPED).
	_, err = svc.StopTask(&ecs.StopTaskInput{Cluster: aws.String("web"), Task: aws.String("t-9")}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, reg.deregistered, 1)
}

func TestService_StopTask_NotFound(t *testing.T) {
	svc, _, _ := serviceTestRig(t)
	_, err := svc.StopTask(&ecs.StopTaskInput{Cluster: aws.String("web"), Task: aws.String("ghost")}, testAccountID)
	require.Error(t, err)
}

// A service with a target group registers each task's ENI IP on RUNNING and
// deregisters it on STOPPED (ecs-v1.md Q8, single-writer).
func TestService_TargetGroup_RegisterOnRunningDeregisterOnStopped(t *testing.T) {
	svc, reg, kv := serviceTestRig(t)
	const tgARN = "arn:aws:elasticloadbalancing:ap-southeast-2:123456789012:targetgroup/web/abc"

	_, err := svc.CreateService(&ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"),
		DesiredCount: aws.Int64(0),
		LoadBalancers: []*ecs.LoadBalancer{{
			TargetGroupArn: aws.String(tgARN), ContainerName: aws.String("app"), ContainerPort: aws.Int64(80),
		}},
	}, testAccountID)
	require.NoError(t, err)

	// Seed an awsvpc service task with an ENI IP (bypasses live ENI provisioning).
	task := &TaskRecord{
		TaskID: "t-1", Cluster: "web", Group: serviceTaskGroup("web"),
		ContainerInstanceID: "i-1", LastStatus: TaskStatusPending,
		NetworkMode: NetworkModeAwsvpc, ENIPrivateIP: "10.0.1.5",
	}
	require.NoError(t, putJSON(kv, TaskKey("web", "t-1"), task))

	// RUNNING → register.
	require.NoError(t, svc.recordTaskState(&bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1",
		TaskID: "t-1", LastStatus: bus.TaskStatusRunning,
	}))
	require.Equal(t, []string{key(tgARN, "10.0.1.5", 80)}, reg.registered)
	assert.Empty(t, reg.deregistered)

	// STOPPED → deregister.
	require.NoError(t, svc.recordTaskState(&bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1",
		TaskID: "t-1", LastStatus: bus.TaskStatusStopped,
	}))
	assert.Equal(t, []string{key(tgARN, "10.0.1.5", 80)}, reg.deregistered)
}
