package awsapi

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertInvalidParameter(t *testing.T, err error, wantMsg string) {
	t.Helper()
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "err = %v", err)
	assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
	assert.Equal(t, wantMsg, msg)
}

const (
	weightMsg     = "The specified weight value '1001' is invalid. Specify an integer between 0 and 1000 and try again."
	baseHighMsg   = "The specified base value '100001' is invalid. Specify an integer between 0 and 100000 and try again."
	baseNegMsg    = "The specified base value '-1' is invalid. Specify an integer between 0 and 100000 and try again."
	validStrategy = `[{"capacityProvider":"cp","weight":1000,"base":100000},{"capacityProvider":"cp2","weight":0,"base":0}]`
)

// Out-of-model CreateCluster and PutClusterCapacityProviders values are
// refused before dispatch; valid ones reach dispatch, which fails here only
// because nc is nil.
func TestClusterRequestValidation(t *testing.T) {
	cases := []struct {
		name    string
		handler describeHandler
		body    string
		wantMsg string
	}{
		{"CreateCluster/logging", CreateCluster,
			`{"configuration":{"executeCommandConfiguration":{"logging":"BOGUS"}}}`, "Unknown executeCommandLoggingType 'BOGUS'"},
		{"CreateCluster/settingName", CreateCluster,
			`{"settings":[{"name":"BOGUS","value":"enabled"}]}`, "Invalid setting 'name'"},
		{"CreateCluster/settingValue", CreateCluster,
			`{"settings":[{"name":"containerInsights","value":"BOGUS"}]}`, "Invalid setting 'value'"},
		{"CreateCluster/weight", CreateCluster,
			`{"defaultCapacityProviderStrategy":[{"capacityProvider":"cp","weight":1001}]}`, weightMsg},
		{"CreateCluster/base", CreateCluster,
			`{"defaultCapacityProviderStrategy":[{"capacityProvider":"cp","base":100001}]}`, baseHighMsg},
		{"PutClusterCapacityProviders/weight", PutClusterCapacityProviders,
			`{"cluster":"c","capacityProviders":[],"defaultCapacityProviderStrategy":[{"capacityProvider":"cp","weight":1001}]}`, weightMsg},
		{"PutClusterCapacityProviders/base", PutClusterCapacityProviders,
			`{"cluster":"c","capacityProviders":[],"defaultCapacityProviderStrategy":[{"capacityProvider":"cp","base":-1}]}`, baseNegMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.handler(context.Background(), nil, "123456789012", []byte(tc.body), nil)
			assertInvalidParameter(t, err, tc.wantMsg)
		})
	}

	valid := []struct {
		name    string
		handler describeHandler
		body    string
	}{
		{"CreateCluster/logging/NONE", CreateCluster, `{"configuration":{"executeCommandConfiguration":{"logging":"NONE"}}}`},
		{"CreateCluster/logging/DEFAULT", CreateCluster, `{"configuration":{"executeCommandConfiguration":{"logging":"DEFAULT"}}}`},
		{"CreateCluster/logging/OVERRIDE", CreateCluster, `{"configuration":{"executeCommandConfiguration":{"logging":"OVERRIDE"}}}`},
		{"CreateCluster/settings", CreateCluster,
			`{"settings":[{"name":"containerInsights","value":"enabled"},{"name":"containerInsights","value":"disabled"},{"name":"containerInsights","value":"enhanced"}]}`},
		{"CreateCluster/strategy", CreateCluster, `{"defaultCapacityProviderStrategy":` + validStrategy + `}`},
		{"PutClusterCapacityProviders/strategy", PutClusterCapacityProviders,
			`{"cluster":"c","capacityProviders":["cp","cp2"],"defaultCapacityProviderStrategy":` + validStrategy + `}`},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.handler(context.Background(), nil, "123456789012", []byte(tc.body), nil)
			_, _, refused := awserrors.ResolveErrorDetail(err)
			assert.False(t, refused, "err = %v", err)
		})
	}
}

func strategy(weight, base int64) []*ecs.CapacityProviderStrategyItem {
	return []*ecs.CapacityProviderStrategyItem{{CapacityProvider: aws.String("cp"), Weight: aws.Int64(weight), Base: aws.Int64(base)}}
}

func TestRunTask_CapacityProviderStrategyRange(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithoutRole(), runOut: &ecs.RunTaskOutput{}}
	_, err := runTask(context.Background(), svc, "123456789012",
		&ecs.RunTaskInput{TaskDefinition: aws.String("web:1"), CapacityProviderStrategy: strategy(1001, 0)}, allowCheck(t, testTaskRoleARN))
	assertInvalidParameter(t, err, weightMsg)
	_, err = runTask(context.Background(), svc, "123456789012",
		&ecs.RunTaskInput{TaskDefinition: aws.String("web:1"), CapacityProviderStrategy: strategy(1, 100001)}, allowCheck(t, testTaskRoleARN))
	assertInvalidParameter(t, err, baseHighMsg)
	assert.False(t, svc.runCalled)

	_, err = runTask(context.Background(), svc, "123456789012",
		&ecs.RunTaskInput{TaskDefinition: aws.String("web:1"), CapacityProviderStrategy: strategy(1000, 100000)}, allowCheck(t, testTaskRoleARN))
	require.NoError(t, err)
	assert.True(t, svc.runCalled)
}

func TestCreateService_CapacityProviderStrategyRange(t *testing.T) {
	svc := &fakeECSService{describeOut: taskDefWithoutRole(), createOut: &ecs.CreateServiceOutput{}}
	_, err := createService(context.Background(), svc, "123456789012",
		&ecs.CreateServiceInput{TaskDefinition: aws.String("web:1"), CapacityProviderStrategy: strategy(1001, 0)}, allowCheck(t, testTaskRoleARN))
	assertInvalidParameter(t, err, weightMsg)
	_, err = createService(context.Background(), svc, "123456789012",
		&ecs.CreateServiceInput{TaskDefinition: aws.String("web:1"), CapacityProviderStrategy: strategy(0, -1)}, allowCheck(t, testTaskRoleARN))
	assertInvalidParameter(t, err, baseNegMsg)
	assert.False(t, svc.createCalled)

	_, err = createService(context.Background(), svc, "123456789012",
		&ecs.CreateServiceInput{TaskDefinition: aws.String("web:1"), CapacityProviderStrategy: strategy(0, 0)}, allowCheck(t, testTaskRoleARN))
	require.NoError(t, err)
	assert.True(t, svc.createCalled)
}

// AWS resolves the task definition before range-checking the strategy, so a
// missing task definition wins over a bad weight.
func TestLaunch_TaskDefinitionResolvedBeforeStrategy(t *testing.T) {
	svc := &fakeECSService{describeErr: errors.New(awserrors.ErrorECSClientException)}
	_, err := runTask(context.Background(), svc, "123456789012",
		&ecs.RunTaskInput{TaskDefinition: aws.String("ghost:1"), CapacityProviderStrategy: strategy(1001, 0)}, allowCheck(t, testTaskRoleARN))
	assertTaskDefinitionNotFound(t, err)
	_, err = createService(context.Background(), svc, "123456789012",
		&ecs.CreateServiceInput{TaskDefinition: aws.String("ghost:1"), CapacityProviderStrategy: strategy(1001, 0)}, allowCheck(t, testTaskRoleARN))
	assertTaskDefinitionNotFound(t, err)
	assert.False(t, svc.runCalled)
	assert.False(t, svc.createCalled)
}

func assertTaskDefinitionNotFound(t *testing.T, err error) {
	t.Helper()
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "err = %v", err)
	assert.Equal(t, awserrors.ErrorECSClientException, code)
	assert.Equal(t, "TaskDefinition not found.", msg)
}
