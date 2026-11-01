package pipeline

import (
	"encoding/json"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestECSRegisterTaskDefinition_DenialRendersAccessDenied403 proves shared
// policy denials stay opaque while retaining the service's 403 envelope.
func TestECSRegisterTaskDefinition_DenialRendersAccessDenied403(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return gwfix.AllowPolicyResource("ecs:RegisterTaskDefinition", "*"), nil
		},
	}
	gw := withRealAuth(gwfix.WithDeps(&Config{DisableLogging: true}, gwfix.Deps{IAM: mock}))
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
