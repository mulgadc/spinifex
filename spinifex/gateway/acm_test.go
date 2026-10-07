package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupACMRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	ctx := context.WithValue(req.Context(), ctxService, "acm")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	return withTestIdentity(req.WithContext(ctx))
}

func TestACMRequest_MissingTarget(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	w := httptest.NewRecorder()
	err := gw.serveACM(w, setupACMRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestACMRequest_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	w := httptest.NewRecorder()
	err := gw.serveACM(w, setupACMRequest("CertificateManager.BogusAction", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A known action with no NATS connection passes routing + policy and fails at
// the NATS-availability guard.
func TestACMRequest_KnownActionNoNATS(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	w := httptest.NewRecorder()
	err := gw.serveACM(w, setupACMRequest("CertificateManager.ListCertificates", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
