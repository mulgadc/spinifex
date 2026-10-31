package gateway

import (
	"encoding/json"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// An empty or non-JSON body is InvalidRequest on every body-reading method
// that parses one, and MissingParameter where a required field is absent.
func TestOperatorAdmin_BodyErrors(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), func(gw *GatewayConfig) {
		withQuota(gw, admissionquota.New(admissionquota.Limits{Enabled: true}, nil))
	})

	tests := []struct {
		method, body string
		wantStatus   int
		wantCode     string
	}{
		{"CreateAccount", `{}`, http.StatusBadRequest, "MissingParameter"},
		{"CreateAccount", `not json`, http.StatusBadRequest, "InvalidRequest"},
		{"DeleteAccount", `{}`, http.StatusBadRequest, "MissingParameter"},
		{"DescribeAccountDeletion", `{}`, http.StatusBadRequest, "MissingParameter"},
		{"GetAccountQuota", `{}`, http.StatusBadRequest, "InvalidRequest"},
		{"PutAccountQuota", `{}`, http.StatusBadRequest, "InvalidRequest"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.body, func(t *testing.T) {
			resp := adminCall(t, app, http.MethodPost, tc.method, "spinifex", tc.body)
			require.Equal(t, tc.wantStatus, resp.StatusCode)
			require.Equal(t, tc.wantCode, adminError(t, resp).Error.Code)
		})
	}
}

// Each gate denies with the same AccessDenied: wrong credential scope, wrong
// account, and a super-admin user holding no spinifex:<Method> grant.
func TestOperatorAdmin_EveryGateDeniesWithTheSameAnswer(t *testing.T) {
	admin := awsidentifiers.DefaultAccountID()
	tests := []struct {
		name    string
		account string
		docs    []iamdomain.PolicyDocument
		service string
	}{
		{"credential scope is not spinifex", admin, allowAllOperatorDocs(), "ec2"},
		{"not the super-admin account", authzAccountID, allowAllOperatorDocs(), "spinifex"},
		{"no spinifex grant", admin, nil, "spinifex"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, app := operatorPipelineApp(t, tc.account, tc.docs, nil)
			for _, method := range operatorv1.AdminMethodNames() {
				resp := adminCall(t, app, http.MethodPost, method, tc.service, `{}`)
				require.Equal(t, http.StatusForbidden, resp.StatusCode, method)
				e := adminError(t, resp)
				require.Equal(t, "AccessDenied", e.Error.Code, method)
				require.Equal(t, "User is not authorized to perform this action.", e.Error.Message, method)
			}
		})
	}
}

func TestOperatorAdmin_GetAccountQuota_DefaultsComeFromConfig(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), func(gw *GatewayConfig) {
		withQuota(gw, admissionquota.New(admissionquota.Limits{Enabled: true}, nil))
	})
	resp := adminCall(t, app, http.MethodPost, "GetAccountQuota", "spinifex", `{"accountId":"000000000002"}`)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		AccountID string            `json:"accountId"`
		Source    map[string]string `json:"source"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "000000000002", out.AccountID)
	require.Equal(t, "config", out.Source["vcpus"])
}

func TestOperatorAdmin_ListAccounts_JSONSuccess(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)
	resp := adminCall(t, app, http.MethodPost, "ListAccounts", "spinifex", `{}`)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	require.NotEmpty(t, resp.Header.Get("X-Amzn-Requestid"), "success carries the request ID header too")
	require.JSONEq(t, `{"accounts":[`+
		`{"accountId":"000000000001","accountName":"spinifex","status":"ACTIVE","createdAt":"2026-01-02T03:04:05Z"},`+
		`{"accountId":"000000000002","accountName":"tenant","status":"ACTIVE","createdAt":"2026-02-03T04:05:06Z"}],`+
		`"count":2}`, opBody(t, resp))
}

// Unlike the Query surface, /admin authorizes first and only then checks the
// cluster, so NATS down is visible only to an authorized caller.
func TestOperatorAdmin_NATSDownAnswers503OnlyAfterAuthorization(t *testing.T) {
	down := func(gw *GatewayConfig) { gw.NATSConn = nil }

	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), down)
	resp := adminCall(t, app, http.MethodPost, "ListAccounts", "spinifex", `{}`)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	e := adminError(t, resp)
	require.Equal(t, "ServiceUnavailable", e.Error.Code)
	require.Equal(t, "cluster unavailable: NATS disconnected — check daemon /local/status", e.Error.Message)

	_, app = operatorPipelineApp(t, authzAccountID, allowAllOperatorDocs(), down)
	resp = adminCall(t, app, http.MethodPost, "ListAccounts", "spinifex", `{}`)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestOperatorAdmin_OversizedBodyIsInvalidRequest(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)
	resp := adminCall(t, app, http.MethodPost, "ListAccounts", "spinifex", strings.Repeat("a", 9000))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	e := adminError(t, resp)
	require.Equal(t, "InvalidRequest", e.Error.Code)
	require.Equal(t, "request body exceeds 8 KiB", e.Error.Message)
}

// Every request, success or failure, gets its own request ID.
func TestOperatorAdmin_RequestIDIsUniquePerRequest(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)

	first := adminCall(t, app, http.MethodPost, "ListAccounts", "spinifex", `{}`).Header.Get("X-Amzn-Requestid")
	second := adminCall(t, app, http.MethodPost, "ListAccounts", "spinifex", `{}`).Header.Get("X-Amzn-Requestid")
	require.NotEmpty(t, first)
	require.NotEqual(t, first, second)
}

// The method is checked before authorization, so an unknown name and a
// non-POST verb answer the same way for a caller with no grant at all.
func TestOperatorAdmin_UnknownMethodAndVerbBeforeAuthorization(t *testing.T) {
	_, app := operatorPipelineApp(t, authzAccountID, nil, nil)

	resp := adminCall(t, app, http.MethodPost, "PurgeEverything", "spinifex", `{}`)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "InvalidAction", adminError(t, resp).Error.Code)

	resp = adminCall(t, app, http.MethodGet, "ListAccounts", "spinifex", "")
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	require.Equal(t, "MethodNotAllowed", adminError(t, resp).Error.Code)
}

// Every action named in spinifexAdminActions refuses a caller outside the
// super-admin account, and refuses it with the same AccessDenied.
func TestOperatorQuery_EveryAdminActionDeniesNonAdminAccount(t *testing.T) {
	_, app := operatorPipelineApp(t, authzAccountID, allowAllOperatorDocs(), nil)

	for action := range spinifexAdminActions {
		t.Run(action, func(t *testing.T) {
			resp := doRequest(app, spinifexQuery(t, action, nil))
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.Equal(t, "AccessDenied", ec2ErrorCode(t, resp))
		})
	}
}

// A storage-config request nobody answers surfaces as the unmapped-error
// InternalError, not a distinct unavailable code.
func TestOperatorQuery_GetStorageStatusWithNoResponder(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)
	resp := doRequest(app, spinifexQuery(t, "GetStorageStatus", nil))

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	require.Equal(t, "InternalError", ec2ErrorCode(t, resp))
}

// Successes are JSON with application/json even though the service is a
// Query-protocol one, and no request ID header is set on this surface.
func TestOperatorQuery_GetVersion_JSONSuccess(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)
	resp := doRequest(app, spinifexQuery(t, "GetVersion", nil))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	require.Empty(t, resp.Header.Get("X-Amzn-Requestid"))
	require.JSONEq(t,
		`{"version":"v9.9.9-test","commit":"abc1234","os":"linux","arch":"amd64","license":"open-source"}`,
		opBody(t, resp))
}

func TestOperatorQuery_ListOchreCatalog_ReturnsEntries(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)
	resp := doRequest(app, spinifexQuery(t, "ListOchreCatalog", nil))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out.Entries)
	require.Contains(t, out.Entries[0], "modelId")
}

func TestOperatorQuery_MalformedQueryString(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)
	resp := doRequest(app, signOperatorRequest(t, http.MethodPost, "/", "spinifex", []byte("Action=%zz")))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "MalformedQueryString", ec2ErrorCode(t, resp))
}

// Without a grant store the model-access actions answer ServerInternal before
// they look at their parameters.
func TestOperatorQuery_ModelAccessWithoutStoreIsServerInternal(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)

	for _, action := range []string{"GrantModelAccess", "RevokeModelAccess", "ListModelAccess"} {
		t.Run(action, func(t *testing.T) {
			resp := doRequest(app, spinifexQuery(t, action, url.Values{"AccountId": {"000000000002"}, "ModelId": {"m"}}))
			require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
			require.Equal(t, "ServerInternal", ec2ErrorCode(t, resp))
		})
	}
}

// The cluster-unavailable check is in the shared Request path, ahead of
// authorization, so even a caller with no grant sees 503 when NATS is down.
func TestOperatorQuery_NATSDownAnswers503BeforeAuthorization(t *testing.T) {
	_, app := operatorPipelineApp(t, authzAccountID, nil, func(gw *GatewayConfig) { gw.NATSConn = nil })
	resp := doRequest(app, spinifexQuery(t, "GetVersion", nil))

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "ServiceUnavailable", ec2ErrorCode(t, resp))
}

// With no node answering, discovery falls back to the expected-node count and
// the listings are empty rather than an error.
func TestOperatorQuery_NodeAndVMListingsWithNoNodes(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)

	resp := doRequest(app, spinifexQuery(t, "GetNodes", nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"nodes":[],"cluster_mode":"single-node"}`, opBody(t, resp))

	resp = doRequest(app, spinifexQuery(t, "GetVMs", nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"vms":[]}`, opBody(t, resp))
}

// The policy gate runs first: a super-admin caller with no spinifex:<Action>
// grant is denied, whatever the action.
func TestOperatorQuery_PolicyDenialBeatsEverythingForTheAdminAccount(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), nil, nil)

	for _, action := range []string{"GetVersion", "PromoteImage", "NotAnAction"} {
		t.Run(action, func(t *testing.T) {
			resp := doRequest(app, spinifexQuery(t, action, nil))
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.Equal(t, "AccessDenied", ec2ErrorCode(t, resp))
		})
	}
}

func TestOperatorQuery_RequiredParameters(t *testing.T) {
	_, app := operatorPipelineApp(t, awsidentifiers.DefaultAccountID(), allowAllOperatorDocs(), nil)

	resp := doRequest(app, spinifexQuery(t, "PromoteImage", nil))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "MissingParameter", ec2ErrorCode(t, resp))
}

// An action outside the admin set is not account-gated, so a non-admin caller
// holding the policy gets the unknown-action answer, not a denial.
func TestOperatorQuery_UnknownActionAndMissingAction(t *testing.T) {
	_, app := operatorPipelineApp(t, authzAccountID, allowAllOperatorDocs(), nil)

	resp := doRequest(app, spinifexQuery(t, "NotAnAction", nil))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "InvalidAction", ec2ErrorCode(t, resp))

	resp = doRequest(app, spinifexQuery(t, "", url.Values{"Version": {"1"}}))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "MissingAction", ec2ErrorCode(t, resp))
}
