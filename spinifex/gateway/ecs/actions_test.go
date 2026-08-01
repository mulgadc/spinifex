package gateway_ecs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTaskRoleARN = "arn:aws:iam::123456789012:role/ecs-task-role"

// fakeECSService is a minimal handlers_ecs.ECSService test double. Embedding
// the (nil) interface satisfies the contract; any method not overridden below
// nil-panics if called, surfacing an unexpected call the same way the EC2
// gateway tests' fakeIAMService does.
type fakeECSService struct {
	handlers_ecs.ECSService

	describeOut *ecs.DescribeTaskDefinitionOutput
	describeErr error

	runOut    *ecs.RunTaskOutput
	runErr    error
	runCalled bool

	startOut    *ecs.StartTaskOutput
	startErr    error
	startCalled bool

	createOut    *ecs.CreateServiceOutput
	createErr    error
	createCalled bool

	updateOut    *ecs.UpdateServiceOutput
	updateErr    error
	updateCalled bool
}

func (f *fakeECSService) DescribeTaskDefinition(_ context.Context, _ *ecs.DescribeTaskDefinitionInput, _ string) (*ecs.DescribeTaskDefinitionOutput, error) {
	return f.describeOut, f.describeErr
}

func (f *fakeECSService) RunTask(_ context.Context, _ *ecs.RunTaskInput, _ string) (*ecs.RunTaskOutput, error) {
	f.runCalled = true
	return f.runOut, f.runErr
}

func (f *fakeECSService) StartTask(_ context.Context, _ *ecs.StartTaskInput, _ string) (*ecs.StartTaskOutput, error) {
	f.startCalled = true
	return f.startOut, f.startErr
}

func (f *fakeECSService) CreateService(_ context.Context, _ *ecs.CreateServiceInput, _ string) (*ecs.CreateServiceOutput, error) {
	f.createCalled = true
	return f.createOut, f.createErr
}

func (f *fakeECSService) UpdateService(_ context.Context, _ *ecs.UpdateServiceInput, _ string) (*ecs.UpdateServiceOutput, error) {
	f.updateCalled = true
	return f.updateOut, f.updateErr
}

// taskDefWithRole is a DescribeTaskDefinition response naming testTaskRoleARN
// as both the task role and the execution role.
func taskDefWithRole() *ecs.DescribeTaskDefinitionOutput {
	return &ecs.DescribeTaskDefinitionOutput{
		TaskDefinition: &ecs.TaskDefinition{
			TaskRoleArn:      aws.String(testTaskRoleARN),
			ExecutionRoleArn: aws.String(testTaskRoleARN),
		},
	}
}

// taskDefWithoutRole is a DescribeTaskDefinition response naming no role —
// the compatibility case that must keep working unchanged.
func taskDefWithoutRole() *ecs.DescribeTaskDefinitionOutput {
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: &ecs.TaskDefinition{}}
}

func denyCheck(t *testing.T, wantARN string) PassRoleChecker {
	t.Helper()
	return func(roleARN string) error {
		assert.Equal(t, wantARN, roleARN)
		return errors.New(awserrors.ErrorAccessDenied)
	}
}

func allowCheck(t *testing.T, wantARN string) PassRoleChecker {
	t.Helper()
	return func(roleARN string) error {
		assert.Equal(t, wantARN, roleARN)
		return nil
	}
}

func mustNotBeCalledCheck(t *testing.T) PassRoleChecker {
	t.Helper()
	return func(roleARN string) error {
		t.Errorf("PassRole must not be checked when no role is named (got %q)", roleARN)
		return nil
	}
}

// --- checkTaskDefinitionRoles / checkTaskLaunchRoles ------------------------

func TestCheckTaskDefinitionRoles_NilCheckerSkips(t *testing.T) {
	assert.NoError(t, checkTaskDefinitionRoles(nil, aws.String(testTaskRoleARN)))
}

func TestCheckTaskDefinitionRoles_EmptyARNSkipped(t *testing.T) {
	assert.NoError(t, checkTaskDefinitionRoles(mustNotBeCalledCheck(t), aws.String(""), nil))
}

func TestCheckTaskDefinitionRoles_Denied(t *testing.T) {
	err := checkTaskDefinitionRoles(denyCheck(t, testTaskRoleARN), aws.String(testTaskRoleARN))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestCheckTaskDefinitionRoles_Allowed(t *testing.T) {
	assert.NoError(t, checkTaskDefinitionRoles(allowCheck(t, testTaskRoleARN), aws.String(testTaskRoleARN)))
}

func TestCheckTaskLaunchRoles_NilCheckerSkipsDescribe(t *testing.T) {
	svc := &fakeECSService{describeErr: errors.New("must not be called")}
	assert.NoError(t, checkTaskLaunchRoles(context.Background(), svc, "123456789012", aws.String("family:1"), nil))
}

func TestCheckTaskLaunchRoles_DescribeErrorPropagates(t *testing.T) {
	svc := &fakeECSService{describeErr: errors.New(awserrors.ErrorECSInvalidParameter)}
	err := checkTaskLaunchRoles(context.Background(), svc, "123456789012", aws.String("ghost:1"), allowCheck(t, testTaskRoleARN))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorECSInvalidParameter, err.Error())
}

// --- RegisterTaskDefinition --------------------------------------------------

func registerTaskDefBody(t *testing.T, taskRoleARN string) []byte {
	t.Helper()
	input := &ecs.RegisterTaskDefinitionInput{Family: aws.String("web")}
	if taskRoleARN != "" {
		input.TaskRoleArn = aws.String(taskRoleARN)
	}
	body, err := json.Marshal(input)
	require.NoError(t, err)
	return body
}

func TestRegisterTaskDefinition_DeniedWithoutPassRole(t *testing.T) {
	_, err := RegisterTaskDefinition(context.Background(), nil, "123456789012",
		registerTaskDefBody(t, testTaskRoleARN), denyCheck(t, testTaskRoleARN))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestRegisterTaskDefinition_AllowedWithPassRole(t *testing.T) {
	// nc is nil, so once the PassRole check passes the NATS dispatch fails —
	// any error here must not be AccessDenied, proving the check let it through.
	_, err := RegisterTaskDefinition(context.Background(), nil, "123456789012",
		registerTaskDefBody(t, testTaskRoleARN), allowCheck(t, testTaskRoleARN))
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestRegisterTaskDefinition_NoRoleSkipsPassRole(t *testing.T) {
	_, err := RegisterTaskDefinition(context.Background(), nil, "123456789012",
		registerTaskDefBody(t, ""), mustNotBeCalledCheck(t))
	require.Error(t, err) // still fails downstream (nil NATS conn), just not on PassRole
	assert.NotEqual(t, awserrors.ErrorAccessDenied, err.Error())
}

// --- runTask / startTask -----------------------------------------------------

func TestRunTask_DeniedWithoutPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole()}
	_, err := runTask(context.Background(), svc, "123456789012",
		&ecs.RunTaskInput{TaskDefinition: aws.String("web:1")}, denyCheck(t, testTaskRoleARN))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
	assert.False(t, svc.runCalled, "RunTask must not dispatch when PassRole is denied")
}

func TestRunTask_AllowedWithPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole(), runOut: &ecs.RunTaskOutput{}}
	_, err := runTask(context.Background(), svc, "123456789012",
		&ecs.RunTaskInput{TaskDefinition: aws.String("web:1")}, allowCheck(t, testTaskRoleARN))
	require.NoError(t, err)
	assert.True(t, svc.runCalled, "RunTask must dispatch once PassRole is granted")
}

func TestRunTask_NoRoleSkipsPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithoutRole(), runOut: &ecs.RunTaskOutput{}}
	_, err := runTask(context.Background(), svc, "123456789012",
		&ecs.RunTaskInput{TaskDefinition: aws.String("web:1")}, mustNotBeCalledCheck(t))
	require.NoError(t, err)
	assert.True(t, svc.runCalled, "a roleless task definition must still launch")
}

func TestStartTask_DeniedWithoutPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole()}
	_, err := startTask(context.Background(), svc, "123456789012",
		&ecs.StartTaskInput{TaskDefinition: aws.String("web:1")}, denyCheck(t, testTaskRoleARN))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
	assert.False(t, svc.startCalled, "StartTask must not dispatch when PassRole is denied")
}

func TestStartTask_AllowedWithPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole(), startOut: &ecs.StartTaskOutput{}}
	_, err := startTask(context.Background(), svc, "123456789012",
		&ecs.StartTaskInput{TaskDefinition: aws.String("web:1")}, allowCheck(t, testTaskRoleARN))
	require.NoError(t, err)
	assert.True(t, svc.startCalled, "StartTask must dispatch once PassRole is granted")
}

func TestStartTask_NoRoleSkipsPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithoutRole(), startOut: &ecs.StartTaskOutput{}}
	_, err := startTask(context.Background(), svc, "123456789012",
		&ecs.StartTaskInput{TaskDefinition: aws.String("web:1")}, mustNotBeCalledCheck(t))
	require.NoError(t, err)
	assert.True(t, svc.startCalled, "a roleless task definition must still launch")
}

// --- createService / updateService ------------------------------------------

func TestCreateService_DeniedWithoutPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole()}
	_, err := createService(context.Background(), svc, "123456789012",
		&ecs.CreateServiceInput{TaskDefinition: aws.String("web:1")}, denyCheck(t, testTaskRoleARN))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
	assert.False(t, svc.createCalled, "CreateService must not dispatch when PassRole is denied")
}

func TestCreateService_AllowedWithPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole(), createOut: &ecs.CreateServiceOutput{}}
	_, err := createService(context.Background(), svc, "123456789012",
		&ecs.CreateServiceInput{TaskDefinition: aws.String("web:1")}, allowCheck(t, testTaskRoleARN))
	require.NoError(t, err)
	assert.True(t, svc.createCalled)
}

func TestUpdateService_DeniedWithoutPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole()}
	_, err := updateService(context.Background(), svc, "123456789012",
		&ecs.UpdateServiceInput{TaskDefinition: aws.String("web:2")}, denyCheck(t, testTaskRoleARN))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
	assert.False(t, svc.updateCalled, "UpdateService must not dispatch when PassRole is denied")
}

func TestUpdateService_AllowedWithPassRole(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithRole(), updateOut: &ecs.UpdateServiceOutput{}}
	_, err := updateService(context.Background(), svc, "123456789012",
		&ecs.UpdateServiceInput{TaskDefinition: aws.String("web:2")}, allowCheck(t, testTaskRoleARN))
	require.NoError(t, err)
	assert.True(t, svc.updateCalled)
}

func TestUpdateService_NoTaskDefinitionSkipsPassRole(t *testing.T) {
	// A desiredCount-only update names no task definition at all.
	svc := &fakeECSService{updateOut: &ecs.UpdateServiceOutput{}}
	_, err := updateService(context.Background(), svc, "123456789012",
		&ecs.UpdateServiceInput{DesiredCount: aws.Int64(3)}, mustNotBeCalledCheck(t))
	require.NoError(t, err)
	assert.True(t, svc.updateCalled)
}
