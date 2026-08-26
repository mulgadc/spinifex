package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestECSRegisterTaskDefinition_PassRole exercises the full gw.ECS_Request
// dispatch (X-Amz-Target parsing, action-level checkPolicy, then the
// iam:PassRole resource check gateway/ecs.go builds and threads through
// gateway_ecs.Handler) with a real authenticated user identity, so the IAM
// policy is actually evaluated rather than taking the pre-IAM bypass.
const testECSPassRoleTargetARN = "arn:aws:iam::123456789012:role/ecs-task-role"

// setupECSUserRequest is setupECSRequest plus an authenticated user identity.
func setupECSUserRequest(target, body string) *http.Request {
	req := setupECSRequest(target, body)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	return req.WithContext(ctx)
}

func registerTaskDefinitionBody(t *testing.T, taskRoleARN string) string {
	t.Helper()
	input := &ecs.RegisterTaskDefinitionInput{Family: aws.String("web")}
	if taskRoleARN != "" {
		input.TaskRoleArn = aws.String(taskRoleARN)
	}
	body, err := json.Marshal(input)
	require.NoError(t, err)
	return string(body)
}

// allowRegisterTaskDefAndPassRole grants ecs:RegisterTaskDefinition
// unconditionally (the action-level check gateway/ecs.go:39 always runs
// first) plus iam:PassRole scoped to passRoleResource, so tests can isolate
// the resource-level PassRole decision from the action-level one.
func allowRegisterTaskDefAndPassRole(passRoleResource string) []handlers_iam.PolicyDocument {
	return []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: "Allow", Action: handlers_iam.StringOrArr{"ecs:RegisterTaskDefinition"}, Resource: handlers_iam.StringOrArr{"*"}},
			{Effect: "Allow", Action: handlers_iam.StringOrArr{"iam:PassRole"}, Resource: handlers_iam.StringOrArr{passRoleResource}},
		},
	}}
}

func TestECSRegisterTaskDefinition_DeniedWithoutPassRole(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			// ecs:RegisterTaskDefinition is granted; iam:PassRole is scoped to a
			// different role, so the target ARN is specifically denied.
			return allowRegisterTaskDefAndPassRole("arn:aws:iam::123456789012:role/some-other-role"), nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := setupECSUserRequest("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition",
		registerTaskDefinitionBody(t, testECSPassRoleTargetARN))

	err := gw.ECS_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assertDenied(t, err)
}

func TestECSRegisterTaskDefinition_AllowedWithPassRole(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return allowRegisterTaskDefAndPassRole(testECSPassRoleTargetARN), nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := setupECSUserRequest("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition",
		registerTaskDefinitionBody(t, testECSPassRoleTargetARN))

	err := gw.ECS_Request(httptest.NewRecorder(), req)
	// PassRole is granted, so the request proceeds past the check to the (nil,
	// disconnected) NATS dispatch; whatever fails downstream, it must not be
	// the PassRole denial.
	require.Error(t, err)
	assertNotDenied(t, err)
}

func TestECSRegisterTaskDefinition_NoRoleSkipsPassRole(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			// No iam:PassRole grant at all — if the gateway still consulted
			// PassRole for a roleless task definition this would deny it.
			return allowPolicyResource("ecs:RegisterTaskDefinition", "*"), nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := setupECSUserRequest("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition",
		registerTaskDefinitionBody(t, ""))

	err := gw.ECS_Request(httptest.NewRecorder(), req)
	require.Error(t, err) // still fails downstream (nil NATS conn), just not on PassRole
	assertNotDenied(t, err)
}

// TestECSRegisterTaskDefinition_DenialRendersAccessDenied403 proves the denial
// reaches the wire as AWS's real PassRole-denial shape: HTTP 403, AWS JSON 1.1
// envelope, the same AccessDenied code/message the EC2 iam:PassRole path uses.
func TestECSRegisterTaskDefinition_DenialRendersAccessDenied403(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return allowPolicyResource("ecs:RegisterTaskDefinition", "*"), nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := setupECSUserRequest("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition",
		registerTaskDefinitionBody(t, testECSPassRoleTargetARN))

	w := httptest.NewRecorder()
	err := gw.ECS_Request(w, req)
	require.Error(t, err)

	gw.ErrorHandler(w, req, err)
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "AccessDeniedException", body.Type)
	assert.Equal(t,
		"User: arn:aws:iam::123456789012:user/alice is not authorized to perform: "+
			"iam:PassRole on resource: "+testECSPassRoleTargetARN, body.Message)
}
