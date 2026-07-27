package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterTaskDefinition_RejectsSecrets covers siv-458: a container secrets[]
// is hard-rejected rather than silently dropped.
func TestRegisterTaskDefinition_RejectsSecrets(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			Secrets: []*ecs.Secret{{
				Name: aws.String("DB_PASSWORD"), ValueFrom: aws.String("arn:aws:ssm:::parameter/db"),
			}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterException")
}

// TestRegisterTaskDefinition_AcceptsUnsupportedLogDriver covers siv-455/458: a
// non-json-file driver is accepted for parity (warned, not rejected) and the
// driver round-trips through DescribeTaskDefinition.
func TestRegisterTaskDefinition_AcceptsUnsupportedLogDriver(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			LogConfiguration: &ecs.LogConfiguration{
				LogDriver: aws.String("awslogs"),
				Options:   map[string]*string{"awslogs-group": aws.String("/ecs/app")},
			},
		}},
	}, testAccountID)
	require.NoError(t, err)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("app"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, d.TaskDefinition.ContainerDefinitions, 1)
	lc := d.TaskDefinition.ContainerDefinitions[0].LogConfiguration
	require.NotNil(t, lc)
	assert.Equal(t, "awslogs", aws.StringValue(lc.LogDriver))
	assert.Equal(t, "/ecs/app", aws.StringValue(lc.Options["awslogs-group"]))
}

// TestRunTask_AssignCarriesExecutionRoleAndLogDriver covers siv-459 (execution
// role plumbed to the agent) and siv-455 (log driver reaches the assign).
func TestRunTask_AssignCarriesExecutionRoleAndLogDriver(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	execARN := "arn:aws:iam::123456789012:role/exec-app"
	_, err = svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family:           aws.String("app"),
		ExecutionRoleArn: aws.String(execARN),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"),
			Cpu: aws.Int64(128), Memory: aws.Int64(256), Essential: aws.Bool(true),
			LogConfiguration: &ecs.LogConfiguration{LogDriver: aws.String(LogDriverJSONFile)},
		}},
	}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, execARN, aws.StringValue(d.TaskDefinition.ExecutionRoleArn))

	_, err = svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)

	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	assert.Equal(t, execARN, poll.Assignments[0].ExecutionRoleARN)
	require.Len(t, poll.Assignments[0].Containers, 1)
	assert.Equal(t, LogDriverJSONFile, poll.Assignments[0].Containers[0].LogDriver)
}

// TestRunTask_AssignCarriesGPU covers mulga-11opz (Epic C task C1): a
// resourceRequirements GPU count on the task def is threaded end-to-end —
// conv -> ContainerDef -> task-level aggregate (TaskRecord.GPU) -> the
// AssignContainer the agent polls for.
func TestRunTask_AssignCarriesGPU(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	_, err = svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("gpu-app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{
			{
				Name: aws.String("trainer"), Image: aws.String("registry/trainer:1"),
				Cpu: aws.Int64(512), Memory: aws.Int64(1024), Essential: aws.Bool(true),
				ResourceRequirements: []*ecs.ResourceRequirement{
					{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String("1")},
				},
			},
			{
				Name: aws.String("sidecar"), Image: aws.String("registry/sidecar:1"),
				Cpu: aws.Int64(64), Memory: aws.Int64(128), Essential: aws.Bool(false),
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	registerInstanceGPU(t, svc, "web", "i-1", 1024, 2048, 1)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("gpu-app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)

	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	require.Len(t, poll.Assignments[0].Containers, 2)
	assert.Equal(t, 1, poll.Assignments[0].Containers[0].GPU)
	assert.Zero(t, poll.Assignments[0].Containers[1].GPU)

	taskID := taskShortID(aws.StringValue(out.Tasks[0].TaskArn))
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec TaskRecord
	found, err := getJSON(t.Context(), kv, TaskKey("web", taskID), &rec)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, rec.GPU)
}
