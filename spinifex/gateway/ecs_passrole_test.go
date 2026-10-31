package gateway

import (
	"encoding/json"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestECSRegisterTaskDefinition_PassRole exercises the full gw.ECS_Request
// dispatch (X-Amz-Target parsing, action-level checkPolicy, then the
// iam:PassRole resource check gateway/ecs.go builds and threads through
// ecsawsapi.Handler) with a real authenticated user identity, so the IAM
// policy is actually evaluated rather than taking the pre-IAM bypass.
const testECSPassRoleTargetARN = "arn:aws:iam::123456789012:role/ecs-task-role"

// setupECSUserRequest is setupECSRequest plus an authenticated user identity.
func setupECSUserRequest(target, body string) *http.Request {
	req := setupECSRequest(target, body)
	ctx := withPrincipalField(req.Context(), fieldIdentity, "alice")
	ctx = withPrincipalField(ctx, fieldPrincipalType, principalTypeUser)
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

// TestECSRegisterTaskDefinition_DenialRendersAccessDenied403 proves shared
// policy denials stay opaque while retaining the service's 403 envelope.
func TestECSRegisterTaskDefinition_DenialRendersAccessDenied403(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return gwfix.AllowPolicyResource("ecs:RegisterTaskDefinition", "*"), nil
		},
	}
	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: mock})
	req := setupECSUserRequest("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition",
		registerTaskDefinitionBody(t, testECSPassRoleTargetARN))

	w := httptest.NewRecorder()
	err := gw.serveECS(w, req)
	require.Error(t, err)

	gw.ErrorHandler(w, req, err)
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "AccessDeniedException", body.Type)
	assert.Equal(t, "User is not authorized to perform this action.", body.Message)
}
