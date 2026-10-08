package handlers_ecs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stored keys and field names outlive any move of the code that writes
// them, so a revision written before a relocation must still resolve after it.
func TestTaskDefinition_PersistedKeysAndFieldNames(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String("app"),
		NetworkMode:             aws.String("bridge"),
		Cpu:                     aws.String("256"),
		Memory:                  aws.String("512"),
		TaskRoleArn:             aws.String("arn:aws:iam::123456789012:role/task"),
		ExecutionRoleArn:        aws.String("arn:aws:iam::123456789012:role/exec"),
		RequiresCompatibilities: aws.StringSlice([]string{"EC2"}),
		RuntimePlatform:         &ecs.RuntimePlatform{CpuArchitecture: aws.String("X86_64")},
		Tags:                    []*ecs.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)

	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	latest, err := kv.Get(t.Context(), "taskdef-families/app/latest-rev")
	require.NoError(t, err)
	assert.JSONEq(t, `1`, string(latest.Value()))

	entry, err := kv.Get(t.Context(), "taskdef-families/app/revs/1")
	require.NoError(t, err)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(entry.Value(), &raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{
		"family", "revision", "arn", "networkMode", "cpu", "memory", "taskRoleArn", "executionRoleArn",
		"requiresCompatibilities", "runtimePlatform", "containers", "status", "tags", "registeredAt",
	}, keys)
}

// Every reference form names the same revision, and one that names nothing is
// the caller's InvalidParameterException.
func TestDescribeTaskDefinition_ResolvesEveryReferenceForm(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 128, 256)
	registerTaskDef(t, svc, "app", 128, 256)

	for ref, want := range map[string]int64{
		"app":   2,
		"app:1": 1,
		TaskDefARN(testRegion, testAccountID, "app", 1): 1,
	} {
		out, err := svc.DescribeTaskDefinition(context.Background(),
			&ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(ref)}, testAccountID)
		require.NoError(t, err, ref)
		assert.Equal(t, want, aws.Int64Value(out.TaskDefinition.Revision), ref)
	}

	for _, ref := range []string{"app:3", "ghost", ""} {
		_, err := svc.DescribeTaskDefinition(context.Background(),
			&ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(ref)}, testAccountID)
		require.Error(t, err, ref)
		assert.Equal(t, awserrors.ErrorECSInvalidParameter, err.Error(), ref)
	}
}

// Deregister only marks the revision, so repeating it succeeds and an unknown
// revision is refused.
func TestDeregisterTaskDefinition_RepeatSucceedsAndUnknownIsRefused(t *testing.T) {
	svc, _ := newTestService(t)
	registerTaskDef(t, svc, "app", 128, 256)

	for range 2 {
		out, err := svc.DeregisterTaskDefinition(context.Background(),
			&ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String("app:1")}, testAccountID)
		require.NoError(t, err)
		assert.Equal(t, TaskDefStatusInactive, aws.StringValue(out.TaskDefinition.Status))
	}

	_, err := svc.DeregisterTaskDefinition(context.Background(),
		&ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String("app:9")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorECSInvalidParameter, err.Error())
}

// Present behaviour: resolution for a launch does not read the revision's
// status, so a deregistered revision still starts tasks.
func TestRunTask_AcceptsAnInactiveRevision(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	_, err = svc.DeregisterTaskDefinition(context.Background(),
		&ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String("app:1")}, testAccountID)
	require.NoError(t, err)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app:1"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Tasks, 1)
	assert.Empty(t, out.Failures)
}

// A bare family is resolved again by the launch itself, so the task runs the
// family's latest revision at the moment the daemon reads it.
func TestRunTask_BareFamilyRunsTheLatestRevision(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256)
	registerTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Equal(t, TaskDefARN(testRegion, testAccountID, "app", 2), aws.StringValue(out.Tasks[0].TaskDefinitionArn))
}

// A service pins the revision it resolved at create, so a later register does
// not change what it runs.
func TestCreateService_PinsTheResolvedRevision(t *testing.T) {
	svc, _, _ := serviceTestRig(t)
	out, err := svc.CreateService(context.Background(), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"),
		TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	pinned := TaskDefARN(testRegion, testAccountID, "app", 1)
	assert.Equal(t, pinned, aws.StringValue(out.Service.TaskDefinition))

	registerTaskDef(t, svc, "app", 128, 256)
	d, err := svc.DescribeServices(context.Background(), &ecs.DescribeServicesInput{
		Cluster: aws.String("web"), Services: aws.StringSlice([]string{"web"}),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, d.Services, 1)
	assert.Equal(t, pinned, aws.StringValue(d.Services[0].TaskDefinition))
}
