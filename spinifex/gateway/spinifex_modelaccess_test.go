package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const accessTestModelID = "meta.llama3-2-1b-instruct-v1:0"

// spinifexAccessRequest drives Spinifex_Request with the query args the model
// access actions read, which the Action-only helper cannot carry.
func spinifexAccessRequest(t *testing.T, gw *GatewayConfig, action, accountID string, args url.Values) *httptest.ResponseRecorder {
	t.Helper()
	args.Set("Action", action)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(args.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxAccountID, accountID)
	ctx = context.WithValue(ctx, ctxIdentity, "admin")
	ctx = context.WithValue(ctx, ctxService, "spinifex")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	if err := gw.Spinifex_Request(w, req); err != nil {
		w.WriteHeader(http.StatusForbidden)
		w.WriteString(err.Error())
	}
	return w
}

// accessGateway builds a gateway whose grant store is backed by an embedded
// JetStream server, so the admin actions are exercised against the same store
// the runtime resolves grants from.
func accessGateway(t *testing.T) *GatewayConfig {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return &GatewayConfig{
		DisableLogging:     true,
		BedrockAccessAdmin: gateway_bedrock.NewModelAccessStore(js, 1),
		IAMService:         allowAllIAMService(),
	}
}

// TestSpinifex_ModelAccess_GrantListRevoke walks the operator's whole loop:
// an account starts with nothing, a grant shows up in the listing, and a
// revoke takes it away again.
func TestSpinifex_ModelAccess_GrantListRevoke(t *testing.T) {
	gw := accessGateway(t)
	adminAccount := admin.DefaultAccountID()

	w := spinifexAccessRequest(t, gw, "ListModelAccess", adminAccount, url.Values{"AccountId": {"000000000002"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), accessTestModelID)

	w = spinifexAccessRequest(t, gw, "GrantModelAccess", adminAccount,
		url.Values{"AccountId": {"000000000002"}, "ModelId": {accessTestModelID}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), accessTestModelID)

	w = spinifexAccessRequest(t, gw, "ListModelAccess", adminAccount, url.Values{"AccountId": {"000000000002"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), accessTestModelID)

	w = spinifexAccessRequest(t, gw, "RevokeModelAccess", adminAccount,
		url.Values{"AccountId": {"000000000002"}, "ModelId": {accessTestModelID}})
	require.Equal(t, http.StatusOK, w.Code)

	w = spinifexAccessRequest(t, gw, "ListModelAccess", adminAccount, url.Values{"AccountId": {"000000000002"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), accessTestModelID)
}

// TestSpinifex_ModelAccess_NonAdminDenied is the important one: an account
// that could grant itself models would make the whole access model decorative.
func TestSpinifex_ModelAccess_NonAdminDenied(t *testing.T) {
	gw := accessGateway(t)

	for _, action := range []string{"GrantModelAccess", "RevokeModelAccess", "ListModelAccess"} {
		w := spinifexAccessRequest(t, gw, action, "000000000002",
			url.Values{"AccountId": {"000000000002"}, "ModelId": {accessTestModelID}})
		require.Equal(t, http.StatusForbidden, w.Code, "action %s", action)
		assert.Contains(t, w.Body.String(), awserrors.ErrorAccessDenied, "action %s", action)
	}
}

func TestSpinifex_ModelAccess_MissingParameters(t *testing.T) {
	gw := accessGateway(t)
	adminAccount := admin.DefaultAccountID()

	cases := []struct {
		action string
		args   url.Values
	}{
		{"GrantModelAccess", url.Values{"ModelId": {accessTestModelID}}},
		{"GrantModelAccess", url.Values{"AccountId": {"000000000002"}}},
		{"RevokeModelAccess", url.Values{"ModelId": {accessTestModelID}}},
		{"ListModelAccess", url.Values{}},
	}
	for _, tc := range cases {
		w := spinifexAccessRequest(t, gw, tc.action, adminAccount, tc.args)
		require.Equal(t, http.StatusForbidden, w.Code, "action %s", tc.action)
		assert.Contains(t, w.Body.String(), awserrors.ErrorMissingParameter, "action %s", tc.action)
	}
}

// TestSpinifex_ModelAccess_NoStoreIsServerError covers a gateway built without
// a grant store: the admin actions refuse rather than panicking on nil.
func TestSpinifex_ModelAccess_NoStoreIsServerError(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	adminAccount := admin.DefaultAccountID()

	for _, action := range []string{"GrantModelAccess", "RevokeModelAccess", "ListModelAccess"} {
		w := spinifexAccessRequest(t, gw, action, adminAccount,
			url.Values{"AccountId": {"000000000002"}, "ModelId": {accessTestModelID}})
		require.Equal(t, http.StatusForbidden, w.Code, "action %s", action)
		assert.Contains(t, w.Body.String(), awserrors.ErrorServerInternal, "action %s", action)
	}
}
