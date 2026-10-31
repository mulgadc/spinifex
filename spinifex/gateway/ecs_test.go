package gateway

import (
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An action whose real handler has not yet landed still resolves to the 501 stub.
func TestECSRequest_KnownActionNotImplemented(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()})
	err := gw.serveECS(httptest.NewRecorder(),
		setupECSRequest("AmazonEC2ContainerServiceV20141113.UpdateCluster", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// A request that clears auth but carries no account ID in context is an internal
// fault, not a client error.
func TestECSRequest_MissingAccountID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Amz-Target", "AmazonEC2ContainerServiceV20141113.ListClusters")
	req = withTestIdentity(req.WithContext(reqctx.WithService(req.Context(), "ecs")))

	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()})
	// Account-less requests are rejected by the policy gate, which runs ahead of
	// the handler's own account guard.
	err := gw.serveECS(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestECSRequest_MissingTarget(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()})
	err := gw.serveECS(httptest.NewRecorder(), setupECSRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestECSRequest_UnknownAction(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()})
	err := gw.serveECS(httptest.NewRecorder(),
		setupECSRequest("AmazonEC2ContainerServiceV20141113.MadeUpAction", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}
