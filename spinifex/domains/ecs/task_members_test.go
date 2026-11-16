package ecs

import (
	"context"
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	ecsv1 "github.com/mulgadc/spinifex/contracts/ecs/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ecs/taskdefinition"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var containerARNPattern = regexp.MustCompile(`^arn:aws:ecs:[^:]+:` + testAccountID + `:container/web/[^/]+/[0-9a-f]{8}-[0-9a-f]{4}-8[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// assertAWSTaskMembers checks the members real AWS reported on the task in
// RunTask and in DescribeTasks while pending, running and stopped.
func assertAWSTaskMembers(t *testing.T, task *ecs.Task, containerARN string) {
	t.Helper()
	assert.Equal(t, "128", aws.StringValue(task.Cpu))
	assert.Equal(t, "256", aws.StringValue(task.Memory))
	assert.Equal(t, "family:app", aws.StringValue(task.Group))
	assert.Equal(t, "EC2", aws.StringValue(task.LaunchType))
	assert.Equal(t, "az-1", aws.StringValue(task.AvailabilityZone))
	require.NotNil(t, task.EnableExecuteCommand)
	assert.False(t, *task.EnableExecuteCommand)
	require.NotNil(t, task.Overrides)
	assert.Equal(t, []*ecs.ContainerOverride{{Name: aws.String("app")}}, task.Overrides.ContainerOverrides)
	assert.NotNil(t, task.Overrides.InferenceAcceleratorOverrides)
	assert.Empty(t, task.Overrides.InferenceAcceleratorOverrides)

	require.Len(t, task.Containers, 1)
	c := task.Containers[0]
	assert.Equal(t, containerARN, aws.StringValue(c.ContainerArn))
	assert.Equal(t, aws.StringValue(task.TaskArn), aws.StringValue(c.TaskArn))
	assert.Equal(t, "registry/app:1", aws.StringValue(c.Image))
	assert.Equal(t, "128", aws.StringValue(c.Cpu))
	assert.Equal(t, "256", aws.StringValue(c.Memory))
}

func TestRunTaskAndDescribeTasks_ReportTaskAndContainerMembers(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	require.NoError(t, svc.recordRegister(ctx, &ecsv1.RegisterInstance{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", AZ: "az-1",
		Capacity: ecsv1.InstanceCapacity{CPU: 1024, MemoryMiB: 2048},
	}))

	run, err := svc.RunTask(ctx, &ecs.RunTaskInput{Cluster: aws.String("web"), TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, run.Failures, "AWS answers failures [] on success")
	assert.Empty(t, run.Failures)
	require.Len(t, run.Tasks, 1)
	require.Len(t, run.Tasks[0].Containers, 1)
	containerARN := aws.StringValue(run.Tasks[0].Containers[0].ContainerArn)
	assert.Regexp(t, containerARNPattern, containerARN)
	assertAWSTaskMembers(t, run.Tasks[0], containerARN)

	taskID := TaskShortID(aws.StringValue(run.Tasks[0].TaskArn))
	describe := func(status string) {
		t.Helper()
		out, err := svc.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: aws.String("web"), Tasks: []*string{run.Tasks[0].TaskArn},
		}, testAccountID)
		require.NoError(t, err)
		require.NotNil(t, out.Failures, "AWS answers failures [] on success")
		assert.Empty(t, out.Failures)
		require.Len(t, out.Tasks, 1)
		assert.Equal(t, status, aws.StringValue(out.Tasks[0].LastStatus))
		assertAWSTaskMembers(t, out.Tasks[0], containerARN)
	}
	describe(TaskStatusPending)

	// Agent reports replace the container states; the members must survive.
	for _, status := range []string{TaskStatusRunning, TaskStatusStopped} {
		require.NoError(t, svc.recordTaskState(ctx, &ecsv1.TaskState{
			AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
			LastStatus: status,
			Containers: []ecsv1.ContainerStatus{{Name: "app", Status: status}},
		}))
		describe(status)
	}
}

func TestTaskToAWS_ProjectsRecordAndTaskDefinition(t *testing.T) {
	svc, _ := newTestService(t)
	rec := &TaskRecord{
		TaskID: "t-1", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "t-1"),
		TaskDefFamily: "app", Group: "service:api",
		ReservedCPU: 0, ReservedMemoryMiB: 512,
		Containers: []ContainerState{{Name: "app", Status: TaskStatusRunning}, {Name: "sidecar", Status: TaskStatusRunning}},
	}
	td := &taskdefinition.Record{
		CPU: "1024", Memory: "2048",
		Containers: []taskdefinition.Container{
			{Name: "app", Image: "registry/app:1", MemoryMiB: 512},
			{Name: "sidecar", Image: "registry/side:1", CPU: 64},
		},
	}

	task := svc.taskToAWS(testAccountID, rec, td)
	assert.Equal(t, "service:api", aws.StringValue(task.Group), "an explicit group is kept")
	assert.Equal(t, "1024", aws.StringValue(task.Cpu), "a task-level cpu wins over the container sum")
	assert.Equal(t, "2048", aws.StringValue(task.Memory))
	assert.Nil(t, task.AvailabilityZone, "a record placed before zones were recorded reports none")
	require.Len(t, task.Containers, 2)
	assert.Equal(t, "0", aws.StringValue(task.Containers[0].Cpu), "AWS reports 0 for a container without cpu")
	assert.Equal(t, "512", aws.StringValue(task.Containers[0].Memory))
	assert.Nil(t, task.Containers[1].Memory, "no hard memory limit, no memory member")
	assert.NotEqual(t, task.Containers[0].ContainerArn, task.Containers[1].ContainerArn)

	// The revision is gone: container ARNs remain, its members do not.
	task = svc.taskToAWS(testAccountID, rec, nil)
	assert.Nil(t, task.Cpu, "no task-level cpu and a zero container sum")
	assert.Equal(t, "512", aws.StringValue(task.Memory))
	require.Len(t, task.Containers, 2)
	assert.Regexp(t, `:container/web/t-1/`, aws.StringValue(task.Containers[0].ContainerArn))
	assert.Nil(t, task.Containers[0].Image)
	assert.Nil(t, task.Containers[0].Cpu)
}
