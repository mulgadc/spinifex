package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
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
	return req.WithContext(ctx)
}

func TestACMActionFromTarget(t *testing.T) {
	assert.Equal(t, "ImportCertificate", acmActionFromTarget("CertificateManager.ImportCertificate"))
	assert.Equal(t, "ListCertificates", acmActionFromTarget("ListCertificates"))
	assert.Equal(t, "", acmActionFromTarget(""))
}

func TestACMActionsMap_AllActionsRegistered(t *testing.T) {
	expected := []string{
		"ImportCertificate",
		"DescribeCertificate",
		"ListCertificates",
		"DeleteCertificate",
		"ListTagsForCertificate",
		"AddTagsToCertificate",
		"RemoveTagsFromCertificate",
	}
	for _, action := range expected {
		_, ok := acmActions[action]
		assert.True(t, ok, "action %q should be registered in acmActions", action)
	}
	assert.Len(t, acmActions, len(expected))
}

func TestACMRequest_MissingTarget(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.ACM_Request(w, setupACMRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestACMRequest_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.ACM_Request(w, setupACMRequest("CertificateManager.RequestCertificate", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A known action with no NATS connection passes routing + policy and fails at
// the NATS-availability guard.
func TestACMRequest_KnownActionNoNATS(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.ACM_Request(w, setupACMRequest("CertificateManager.ListCertificates", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
