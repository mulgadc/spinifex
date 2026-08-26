package gateway

//test:in-package — the authorization gates read unexported context keys and
// principal-type constants, and asserting on them through the router alone
// would leave which gate denied untested.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminPathMethod(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"known method", "/admin/CreateAccount", "CreateAccount", true},
		{"deletion method", "/admin/DeleteAccount", "DeleteAccount", true},
		{"unknown method", "/admin/PurgeEverything", "", false},
		{"nested path", "/admin/CreateAccount/extra", "", false},
		{"bare prefix", "/admin/", "", false},
		{"not admin", "/", "", false},
		{"case mismatch", "/admin/createaccount", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := adminPathMethod(tc.path)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// createAccountPolicy grants exactly the action the signup principal holds.
func createAccountPolicy() *policyMockIAMService {
	allow := []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{
				Effect:   "Allow",
				Action:   handlers_iam.StringOrArr{"spinifex:CreateAccount"},
				Resource: handlers_iam.StringOrArr{"*"},
			},
		},
	}}
	return &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return allow, nil },
		getRolePoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return allow, nil },
	}
}

// Every gate must deny on its own, and every denial must be indistinguishable
// from the others so a caller cannot learn which one it failed.
func TestAuthorizeAdminGates(t *testing.T) {
	tests := []struct {
		name          string
		service       string
		accountID     string
		identity      string
		principalType string
		policy        *policyMockIAMService
		wantErr       string
	}{
		{
			name:          "permitted signup principal",
			service:       "spinifex",
			accountID:     admin.DefaultAccountID(),
			identity:      "signup",
			principalType: principalTypeUser,
			policy:        createAccountPolicy(),
		},
		{
			name:          "credential scope is not spinifex",
			service:       "ec2",
			accountID:     admin.DefaultAccountID(),
			identity:      "signup",
			principalType: principalTypeUser,
			policy:        createAccountPolicy(),
			wantErr:       awserrors.ErrorAccessDenied,
		},
		{
			name:          "not the super-admin account",
			service:       "spinifex",
			accountID:     "123456789012",
			identity:      "signup",
			principalType: principalTypeUser,
			policy:        createAccountPolicy(),
			wantErr:       awserrors.ErrorAccessDenied,
		},
		{
			name:          "assumed-role session",
			service:       "spinifex",
			accountID:     admin.DefaultAccountID(),
			identity:      "signup",
			principalType: principalTypeAssumedRole,
			policy:        createAccountPolicy(),
			wantErr:       awserrors.ErrorAccessDenied,
		},
		{
			name:          "no identity",
			service:       "spinifex",
			accountID:     admin.DefaultAccountID(),
			identity:      "",
			principalType: principalTypeUser,
			policy:        createAccountPolicy(),
			wantErr:       awserrors.ErrorAccessDenied,
		},
		{
			name:          "policy does not grant CreateAccount",
			service:       "spinifex",
			accountID:     admin.DefaultAccountID(),
			identity:      "signup",
			principalType: principalTypeUser,
			policy:        &policyMockIAMService{},
			wantErr:       awserrors.ErrorAccessDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := &GatewayConfig{DisableLogging: true, IAMService: tc.policy}
			req := httptest.NewRequest(http.MethodPost, "/admin/CreateAccount", nil)
			ctx := context.WithValue(req.Context(), ctxService, tc.service)
			ctx = context.WithValue(ctx, ctxAccountID, tc.accountID)
			ctx = context.WithValue(ctx, ctxIdentity, tc.identity)
			ctx = context.WithValue(ctx, ctxPrincipalType, tc.principalType)

			err := gw.authorizeAdmin(req.WithContext(ctx), "CreateAccount")
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tc.wantErr, code)
		})
	}
}

// An unauthenticated request carries no auth context at all, which must not
// read as a permitted principal.
func TestAuthorizeAdminDeniesUnauthenticated(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	req := httptest.NewRequest(http.MethodPost, "/admin/CreateAccount", nil)

	err := gw.authorizeAdmin(req, "CreateAccount")
	require.Error(t, err)
	assertDenied(t, err)
}

// adminRequest drives Admin_Request with the chi route param the router sets.
func adminRequest(gw *GatewayConfig, method string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/"+method, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("method", method)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	gw.Admin_Request(rec, req)
	return rec
}

func decodeAdminError(t *testing.T, rec *httptest.ResponseRecorder) adminErrorBody {
	t.Helper()
	var body adminErrorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// An unknown method is rejected before authorization, so it never discloses
// whether the caller would have been permitted.
func TestAdminRequestUnknownMethod(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}

	rec := adminRequest(gw, "PurgeEverything", `{}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, awserrors.ErrorInvalidAction, decodeAdminError(t, rec).Error.Code)
}

func TestAdminRequestDeniesUnauthorizedCaller(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}

	rec := adminRequest(gw, "CreateAccount", `{}`)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, awserrors.ErrorAccessDenied, decodeAdminError(t, rec).Error.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// The request ID ties a client failure to the gateway log line, so it must be
// present on the header and in the body even for a denial.
func TestAdminRequestAlwaysCarriesRequestID(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}

	rec := adminRequest(gw, "CreateAccount", `{}`)

	requestID := rec.Header().Get("X-Amzn-Requestid")
	assert.NotEmpty(t, requestID)
	assert.Equal(t, requestID, decodeAdminError(t, rec).RequestID)
}

// An unmapped code must not escape as a 200 or an empty body.
func TestWriteAdminErrorFallsBackOnUnknownCode(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	rec := httptest.NewRecorder()

	gw.writeAdminError(rec, "req-1", "NotARegisteredCode", "")

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, awserrors.ErrorInternalError, decodeAdminError(t, rec).Error.Code)
}

// The router serves every verb from one pattern, so the handler is what
// refuses a GET — a second route would shadow the POST one.
func TestAdminRequestRejectsNonPost(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	req := httptest.NewRequest(http.MethodGet, "/admin/CreateAccount", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("method", "CreateAccount")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	gw.Admin_Request(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, awserrors.ErrorMethodNotAllowed, decodeAdminError(t, rec).Error.Code)
}

// authorizedAdminRequest builds a request that clears every authorization gate,
// so the guards behind them can be tested on their own.
func authorizedAdminRequest(gw *GatewayConfig, body string) *httptest.ResponseRecorder {
	return authorizedAdminRequestForMethod(gw, "CreateAccount", body)
}

// authorizedAdminRequestForMethod is the same for a named method, so a grant
// that covers one method can be shown not to cover another.
func authorizedAdminRequestForMethod(gw *GatewayConfig, method, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/"+method, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("method", method)

	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, ctxService, "spinifex")
	ctx = context.WithValue(ctx, ctxAccountID, admin.DefaultAccountID())
	ctx = context.WithValue(ctx, ctxIdentity, "admin")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)

	rec := httptest.NewRecorder()
	gw.Admin_Request(rec, req.WithContext(ctx))
	return rec
}

// An authorized caller must still get a clear answer when the cluster is not
// reachable, rather than a panic or a silent success.
func TestAdminRequestReportsClusterUnavailable(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: createAccountPolicy()}

	rec := authorizedAdminRequest(gw, `{}`)

	assert.Equal(t, awserrors.ErrorServiceUnavailable, decodeAdminError(t, rec).Error.Code)
}

// The credential-minting CLI grants these by name. A method served but absent
// from the list would be uncallable by every key an operator issues.
func TestAdminMethodNamesCoverTheRoutedMethods(t *testing.T) {
	names := AdminMethodNames()

	require.Len(t, names, len(adminMethods))
	assert.True(t, sort.StringsAreSorted(names), "names must be sorted for stable help text")
	for _, name := range names {
		method, ok := adminPathMethod(adminPathPrefix + name)
		assert.True(t, ok, "%s is granted but not routed", name)
		assert.Equal(t, name, method)
	}
}
