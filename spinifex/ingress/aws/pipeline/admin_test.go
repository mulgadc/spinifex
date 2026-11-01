package pipeline

//test:in-package — the authorization gates read unexported context keys and
// principal-type constants, and asserting on them through the router alone
// would leave which gate denied untested.

import (
	"context"
	"encoding/json"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAccountPolicy grants exactly the action the signup principal holds.
func createAccountPolicy() *gwfix.PolicyIAM {
	allow := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{
				Effect:   "Allow",
				Action:   iamdomain.StringOrArr{"spinifex:CreateAccount"},
				Resource: iamdomain.StringOrArr{"*"},
			},
		},
	}}
	return &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
		GetRolePoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
	}
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
	gw := withOperator(withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()})))

	rec := adminRequest(gw, "PurgeEverything", `{}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, awserrors.ErrorInvalidAction, decodeAdminError(t, rec).Error.Code)
}

func TestAdminRequestDeniesUnauthorizedCaller(t *testing.T) {
	gw := withOperator(withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()})))

	rec := adminRequest(gw, "CreateAccount", `{}`)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, awserrors.ErrorAccessDenied, decodeAdminError(t, rec).Error.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// The request ID ties a client failure to the gateway log line, so it must be
// present on the header and in the body even for a denial.
func TestAdminRequestAlwaysCarriesRequestID(t *testing.T) {
	gw := withOperator(withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()})))

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
	gw := withOperator(withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()})))
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
	ctx = reqctx.WithService(ctx, "spinifex")
	ctx = withPrincipalField(ctx, fieldAccountID, awsidentifiers.DefaultAccountID())
	ctx = withPrincipalField(ctx, fieldIdentity, "admin")
	ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)

	rec := httptest.NewRecorder()
	gw.Admin_Request(rec, req.WithContext(ctx))
	return rec
}

// An authorized caller must still get a clear answer when the cluster is not
// reachable, rather than a panic or a silent success.
func TestAdminRequestReportsClusterUnavailable(t *testing.T) {
	gw := withOperator(withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: createAccountPolicy()})))

	rec := authorizedAdminRequest(gw, `{}`)

	assert.Equal(t, awserrors.ErrorServiceUnavailable, decodeAdminError(t, rec).Error.Code)
}

// Each admin method is a separate grant. The signup Worker holds
// spinifex:CreateAccount and must not reach deletion with it.
func TestCreateAccountGrantDoesNotAllowDeleteAccount(t *testing.T) {
	gw := withOperator(withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: createAccountPolicy()})))

	for _, method := range []string{"DeleteAccount", "DescribeAccountDeletion", "ListAccounts"} {
		t.Run(method, func(t *testing.T) {
			rec := authorizedAdminRequestForMethod(gw, method, `{}`)

			assert.Equal(t, awserrors.ErrorAccessDenied, decodeAdminError(t, rec).Error.Code)
		})
	}
}
