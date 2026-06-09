package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func spinifexRequest(t *testing.T, gw *GatewayConfig, action, accountID, identity string) *httptest.ResponseRecorder {
	t.Helper()
	body := "Action=" + action
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxAccountID, accountID)
	ctx = context.WithValue(ctx, ctxIdentity, identity)
	ctx = context.WithValue(ctx, ctxService, "spinifex")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	err := gw.Spinifex_Request(w, req)
	if err != nil {
		// Write the error like the real ErrorHandler would
		w.WriteHeader(http.StatusForbidden)
		w.WriteString(err.Error())
	}
	return w
}

func TestSpinifex_GetVersion_Admin(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		Version:        "v0.5.0-43-gae1deb5",
		Commit:         "ae1deb5",
	}
	w := spinifexRequest(t, gw, "GetVersion", admin.DefaultAccountID(), "admin")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"version":"v0.5.0-43-gae1deb5"`)
	assert.Contains(t, w.Body.String(), `"license":"open-source"`)
}

func TestSpinifex_GetVersion_NonAdmin_Denied(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		Version:        "v0.5.0",
		Commit:         "abc123",
	}
	w := spinifexRequest(t, gw, "GetVersion", "000000000002", "alice")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), awserrors.ErrorAccessDenied)
}

func TestSpinifex_GetNodes_NonAdmin_Denied(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := spinifexRequest(t, gw, "GetNodes", "000000000002", "alice")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), awserrors.ErrorAccessDenied)
}

func TestSpinifex_GetVMs_NonAdmin_Denied(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := spinifexRequest(t, gw, "GetVMs", "000000000002", "alice")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), awserrors.ErrorAccessDenied)
}

func TestSpinifex_InvalidAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := spinifexRequest(t, gw, "DoesNotExist", admin.DefaultAccountID(), "admin")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), awserrors.ErrorInvalidAction)
}

func TestSpinifex_MissingAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	ctx := context.WithValue(req.Context(), ctxService, "spinifex")
	ctx = context.WithValue(ctx, ctxAccountID, admin.DefaultAccountID())
	ctx = context.WithValue(ctx, ctxIdentity, "admin")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	err := gw.Spinifex_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestSpinifex_NoAccountID(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	body := "Action=GetVersion"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "spinifex")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	err := gw.Spinifex_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestSpinifex_GetNodes_NoNATS(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		NATSConn:       nil,
	}
	w := spinifexRequest(t, gw, "GetNodes", admin.DefaultAccountID(), "admin")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), awserrors.ErrorServerInternal)
}
