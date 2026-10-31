package gateway

import (
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEKSRequest_UnknownActionReturnsInvalidAction(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()})
	req := httptest.NewRequest(http.MethodGet, "/totally-unknown", nil)
	ctx := reqctx.WithService(req.Context(), "eks")
	ctx = withPrincipalField(ctx, fieldAccountID, "111122223333")
	req = withTestIdentity(req.WithContext(ctx))

	w := httptest.NewRecorder()
	err := gw.serveEKS(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestEKSRequest_MissingNATSReturnsServerInternal(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()})
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	ctx := reqctx.WithService(req.Context(), "eks")
	ctx = withPrincipalField(ctx, fieldAccountID, "111122223333")
	req = withTestIdentity(req.WithContext(ctx))

	w := httptest.NewRecorder()
	err := gw.serveEKS(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestErrorHandler_EKSEmitsJSONNotXML(t *testing.T) {
	gw := withContainers(withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}))
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	ctx := reqctx.WithService(req.Context(), "eks")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	gw.ErrorHandler(w, req, errEKSNotImpl())
	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Equal(t, eksawsapi.JSONContentType, w.Header().Get("Content-Type"))
	assert.True(t, strings.HasPrefix(w.Body.String(), "{"), "expected JSON body, got %q", w.Body.String())

	env := decodeJSONError(t, w.Result())
	assert.Equal(t, "NotImplementedException", env.Type)
	// SDKs resolve the modelled exception type from this header, not the body.
	assert.Equal(t, env.Type, w.Header().Get("X-Amzn-Errortype"))
}

// errEKSNotImpl returns the error a stub EKS handler would surface.
func errEKSNotImpl() error { return errAWS(awserrors.ErrorNotImplemented) }

// awsCodeError lets tests return an awserrors-code error without depending on the impl package.
type awsCodeError struct{ code string }

func (e *awsCodeError) Error() string { return e.code }
func errAWS(code string) error        { return &awsCodeError{code: code} }
