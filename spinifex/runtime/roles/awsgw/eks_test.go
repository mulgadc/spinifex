package awsgw_test

import (
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestErrorHandler_EKSEmitsJSONNotXML(t *testing.T) {
	gw := withContainers(withVerifier(&gateway.GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}))
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
