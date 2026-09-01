//test:in-package — drives EKS_Request through the gateway's unexported test
// helpers (withTestIdentity, policyMockIAMService) and auth context keys.

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchEKS drives the gateway with no NATS connection. A permitted request
// therefore reaches the NATS guard and fails there, which is what proves the
// policy check ran ahead of the resource existing at all.
func dispatchEKS(t *testing.T, gw *GatewayConfig, method, path, body string) error {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, authzAccountID)
	req = withTestIdentity(req.WithContext(ctx))
	return gw.EKS_Request(httptest.NewRecorder(), req)
}

func eksARN(resource string) string {
	return "arn:aws:eks:" + authzRegion + ":" + authzAccountID + ":" + resource
}

// TestEKSRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production cluster; before the resolver the fence was inert and
// DeleteCluster against it was permitted with nothing logged.
func TestEKSRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:DeleteCluster", "arn:aws:eks:*:*:cluster/prod"),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev", ""))
}

// TestEKSRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestEKSRequest_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:DeleteCluster", "arn:aws:eks:*:*:cluster/dev"),
	)

	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev", ""))
	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod", ""))
}

// A nodegroup ARN carries a UUID the gate cannot see, so the resolver leaves it
// wildcarded. The AWS-documented policy spelling must still match.
func TestEKSRequest_NodegroupScopeMatchesWildcardUUID(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:DeleteNodegroup", "arn:aws:eks:*:*:nodegroup/prod/workers/*"),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/workers", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/prod/node-groups/batch", ""))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, "/clusters/dev/node-groups/workers", ""))
}

// CreateCluster names its cluster in the body, so the body read has to happen
// before the gate for a fence on a name prefix to fire at all.
func TestEKSRequest_CreateClusterScopesFromBody(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:CreateCluster", "arn:aws:eks:*:*:cluster/prod-*"),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", `{"name":"prod-1"}`))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", `{"name":"dev-1"}`))
	// An unreadable body authorizes account-wide and stays the handler's fault.
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost, "/clusters", "{not json"))
}

// The access-entry principal arrives percent-encoded in the path; the ARN the
// gate builds must be the one the handler's own builder produces.
func TestEKSRequest_AccessEntryPathIsUnescaped(t *testing.T) {
	const principal = "arn:aws:iam::123456789012:role/app/admin"
	entry := arn.FormatEKSAccessEntry(authzRegion, authzAccountID, "prod", handlers_eks.PrincipalARNHash(principal))

	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:DeleteAccessEntry", entry),
	)

	path := "/clusters/prod/access-entries/" + url.PathEscape(principal)
	assertDenied(t, dispatchEKS(t, gw, http.MethodDelete, path, ""))

	other := "/clusters/prod/access-entries/" + url.PathEscape("arn:aws:iam::123456789012:role/app/reader")
	assertPermitted(t, dispatchEKS(t, gw, http.MethodDelete, other, ""))
}

// A tag request names its resource by ARN. The handler ignores that ARN's
// account and works in the caller's, so the gate must too.
func TestEKSRequest_TagResourceScopesTheNamedResource(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "eks:*", "*"),
		statement("Deny", "eks:TagResource", eksARN("cluster/prod")),
	)

	assertDenied(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape(eksARN("cluster/prod")), `{"tags":{"k":"v"}}`))
	assertPermitted(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape(eksARN("cluster/dev")), `{"tags":{"k":"v"}}`))
	// The same cluster spelled under another account is still the caller's.
	assertDenied(t, dispatchEKS(t, gw, http.MethodPost,
		"/tags/"+url.PathEscape("arn:aws:eks:us-east-1:999999999999:cluster/prod"), `{"tags":{"k":"v"}}`))
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestEKSRequest_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/clusters", nil).
		WithContext(context.WithValue(context.Background(), ctxAccountID, authzAccountID)))

	for _, action := range gateway_eks.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			resources, err := gateway_eks.ResourceARNs(action, authzRegion, authzAccountID,
				[]string{"prod", "workers", "extra"}, []byte(`{"name":"prod","nodegroupName":"workers","addonName":"coredns","principalArn":"arn:aws:iam::123456789012:role/app"}`))
			require.NoError(t, err)
			assert.NoError(t, gw.checkPolicyResources(req, "eks", action, resources))
		})
	}
}

// The internal CP-VM routes name the target account in the path, so eks:* on *
// evaluated as permitted and returned another tenant's cluster state. The
// principal gate now rejects the tenant ahead of the policy check.
func TestEKSRequest_InternalRoutesRejectTenantPrincipal(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))

	assertDenied(t, dispatchEKS(t, gw, http.MethodGet, "/clusters/prod/internal-addons/999988887777", ""))
	assertDenied(t, dispatchEKS(t, gw, http.MethodGet,
		"/clusters/prod/internal-recovery/999988887777/i-0abc", ""))
	// Its own account is no different: a tenant is not a control-plane VM.
	assertDenied(t, dispatchEKS(t, gw, http.MethodGet, "/clusters/prod/internal-addons/"+authzAccountID, ""))
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestEKSRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "eks:*", "*"))
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/clusters", nil).
		WithContext(context.WithValue(context.Background(), ctxService, "eks")))

	err := gw.EKS_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
