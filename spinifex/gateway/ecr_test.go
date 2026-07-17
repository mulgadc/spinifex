package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupECRRequest builds an ECR control-plane request context. It carries a
// full SigV4-shaped identity (not just accountID) so handlers reached through
// it — including GetAuthorizationToken, which now builds a canonical caller
// ARN from the context rather than a best-effort helper — behave exactly as
// they would for a real authenticated user.
func setupECRRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	ctx := context.WithValue(req.Context(), ctxService, "ecr")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxIdentity, "dev")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	ctx = context.WithValue(ctx, ctxAccessKey, "AKIAECRTESTCONTROLPL1")
	return req.WithContext(ctx)
}

func TestECRActionFromTarget(t *testing.T) {
	assert.Equal(t, "CreateRepository",
		ecrActionFromTarget("AmazonEC2ContainerRegistry_V20150921.CreateRepository"))
	assert.Equal(t, "GetAuthorizationToken", ecrActionFromTarget("GetAuthorizationToken"))
	assert.Empty(t, ecrActionFromTarget(""))
}

func TestECRActionsMap_CoreActionsRegistered(t *testing.T) {
	core := []string{
		"GetAuthorizationToken", "CreateRepository", "DeleteRepository",
		"DescribeRepositories", "BatchGetImage", "BatchCheckLayerAvailability",
		"PutImage", "InitiateLayerUpload", "UploadLayerPart", "CompleteLayerUpload",
	}
	for _, action := range core {
		_, ok := gateway_ecrapi.Actions[action]
		assert.True(t, ok, "action %q should be registered", action)
	}
}

func TestECRRequest_MissingTarget(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.ECR_Request(httptest.NewRecorder(), setupECRRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestECRRequest_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.ECR_Request(httptest.NewRecorder(),
		setupECRRequest("AmazonEC2ContainerRegistry_V20150921.MadeUpAction", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A registered-but-unimplemented action resolves to the 501 stub until its
// handler lands. The repo/image actions are now served inline, so
// ListRepositories stands in as a still-stubbed action.
func TestECRRequest_KnownActionNotImplemented(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.ECR_Request(httptest.NewRecorder(),
		setupECRRequest("AmazonEC2ContainerRegistry_V20150921.ListRepositories", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

func TestOCIRegistry_VersionCheck(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	r := chi.NewRouter()
	gw.mountOCIRegistry(r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-Api-Version"))
}

func TestOCIRegistry_StubReturns501(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	r := chi.NewRouter()
	gw.mountOCIRegistry(r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/team/app/tags/list", nil))

	assert.Equal(t, http.StatusNotImplemented, w.Code)

	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Errors, 1)
	assert.Equal(t, "UNSUPPORTED", body.Errors[0].Code)
}
