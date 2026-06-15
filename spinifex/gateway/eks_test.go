package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupEKSAction_ResolvesKnownRoutes(t *testing.T) {
	cases := []struct {
		method, path string
		wantAction   string
		wantParams   []string
	}{
		{"POST", "/clusters", "CreateCluster", nil},
		{"GET", "/clusters", "ListClusters", nil},
		{"GET", "/clusters/alpha", "DescribeCluster", []string{"alpha"}},
		{"DELETE", "/clusters/alpha", "DeleteCluster", []string{"alpha"}},
		{"POST", "/clusters/alpha/update-config", "UpdateClusterConfig", []string{"alpha"}},
		{"POST", "/clusters/alpha/update-version", "UpdateClusterVersion", []string{"alpha"}},
		{"POST", "/clusters/alpha/node-groups", "CreateNodegroup", []string{"alpha"}},
		{"GET", "/clusters/alpha/node-groups", "ListNodegroups", []string{"alpha"}},
		{"GET", "/clusters/alpha/node-groups/ng1", "DescribeNodegroup", []string{"alpha", "ng1"}},
		{"DELETE", "/clusters/alpha/node-groups/ng1", "DeleteNodegroup", []string{"alpha", "ng1"}},
		{"POST", "/clusters/alpha/node-groups/ng1/update-config", "UpdateNodegroupConfig", []string{"alpha", "ng1"}},
		{"POST", "/clusters/alpha/node-groups/ng1/update-version", "UpdateNodegroupVersion", []string{"alpha", "ng1"}},
		{"POST", "/clusters/alpha/access-entries", "CreateAccessEntry", []string{"alpha"}},
		{"GET", "/clusters/alpha/access-entries", "ListAccessEntries", []string{"alpha"}},
		{"GET", "/clusters/alpha/access-entries/arn-user", "DescribeAccessEntry", []string{"alpha", "arn-user"}},
		{"POST", "/clusters/alpha/access-entries/arn-user", "UpdateAccessEntry", []string{"alpha", "arn-user"}},
		{"DELETE", "/clusters/alpha/access-entries/arn-user", "DeleteAccessEntry", []string{"alpha", "arn-user"}},
		{"POST", "/clusters/alpha/access-entries/arn-user/access-policies", "AssociateAccessPolicy", []string{"alpha", "arn-user"}},
		{"DELETE", "/clusters/alpha/access-entries/arn-user/access-policies", "DisassociateAccessPolicy", []string{"alpha", "arn-user"}},
		{"GET", "/clusters/alpha/access-entries/arn-user/access-policies", "ListAssociatedAccessPolicies", []string{"alpha", "arn-user"}},
		{"GET", "/access-policies", "ListAccessPolicies", nil},
		{"GET", "/clusters/alpha/addons", "ListAddons", []string{"alpha"}},
		{"GET", "/addons/supported-versions", "DescribeAddonVersions", nil},
		{"POST", "/clusters/alpha/addons", "CreateAddon", []string{"alpha"}},
		{"GET", "/clusters/alpha/addons/vpc-cni", "DescribeAddon", []string{"alpha", "vpc-cni"}},
		{"DELETE", "/clusters/alpha/addons/vpc-cni", "DeleteAddon", []string{"alpha", "vpc-cni"}},
		{"POST", "/clusters/alpha/addons/vpc-cni/update", "UpdateAddon", []string{"alpha", "vpc-cni"}},
		{"POST", "/clusters/alpha/identity-provider-configs/associate", "AssociateIdentityProviderConfig", []string{"alpha"}},
		{"POST", "/clusters/alpha/identity-provider-configs/describe", "DescribeIdentityProviderConfig", []string{"alpha"}},
		{"POST", "/clusters/alpha/identity-provider-configs/disassociate", "DisassociateIdentityProviderConfig", []string{"alpha"}},
		{"GET", "/clusters/alpha/identity-provider-configs", "ListIdentityProviderConfigs", []string{"alpha"}},
		{"POST", "/tags/arn-cluster", "TagResource", []string{"arn-cluster"}},
		{"DELETE", "/tags/arn-cluster", "UntagResource", []string{"arn-cluster"}},
		{"GET", "/tags/arn-cluster", "ListTagsForResource", []string{"arn-cluster"}},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			action, params, handler, ok := lookupEKSAction(tc.method, tc.path)
			require.True(t, ok, "expected route to match for %s %s", tc.method, tc.path)
			require.NotNil(t, handler)
			assert.Equal(t, tc.wantAction, action)
			assert.Equal(t, tc.wantParams, params)
		})
	}
}

// IAM principal ARNs are percent-encoded in the request path; lookupEKSAction is
// fed EscapedPath() so %2F stays a single segment and is unescaped before the handler.
func TestLookupEKSAction_EncodedPrincipalARN(t *testing.T) {
	const (
		arn     = "arn:aws:iam::000000000001:user/admin"
		escaped = "arn%3Aaws%3Aiam%3A%3A000000000001%3Auser%2Fadmin"
	)
	cases := []struct {
		method, path string
		wantAction   string
	}{
		{"GET", "/clusters/alpha/access-entries/" + escaped, "DescribeAccessEntry"},
		{"POST", "/clusters/alpha/access-entries/" + escaped, "UpdateAccessEntry"},
		{"DELETE", "/clusters/alpha/access-entries/" + escaped, "DeleteAccessEntry"},
		{"POST", "/clusters/alpha/access-entries/" + escaped + "/access-policies", "AssociateAccessPolicy"},
		{"DELETE", "/clusters/alpha/access-entries/" + escaped + "/access-policies", "DisassociateAccessPolicy"},
		{"GET", "/clusters/alpha/access-entries/" + escaped + "/access-policies", "ListAssociatedAccessPolicies"},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.wantAction, func(t *testing.T) {
			action, params, handler, ok := lookupEKSAction(tc.method, tc.path)
			require.True(t, ok, "encoded ARN path should match: %s %s", tc.method, tc.path)
			require.NotNil(t, handler)
			assert.Equal(t, tc.wantAction, action)
			assert.Equal(t, []string{"alpha", arn}, params)
		})
	}
}

func TestLookupEKSAction_CoversAllActions(t *testing.T) {
	expected := map[string]bool{
		"CreateCluster": false,
		// Internal control-plane broker routes, not AWS-SDK EKS actions.
		"PublishInternal":                    false,
		"WebhookTokenReview":                 false,
		"ListInternalAddons":                 false,
		"DescribeCluster":                    false,
		"ListClusters":                       false,
		"UpdateClusterConfig":                false,
		"UpdateClusterVersion":               false,
		"DeleteCluster":                      false,
		"CreateNodegroup":                    false,
		"DescribeNodegroup":                  false,
		"ListNodegroups":                     false,
		"UpdateNodegroupConfig":              false,
		"UpdateNodegroupVersion":             false,
		"DeleteNodegroup":                    false,
		"CreateAccessEntry":                  false,
		"DescribeAccessEntry":                false,
		"ListAccessEntries":                  false,
		"UpdateAccessEntry":                  false,
		"DeleteAccessEntry":                  false,
		"AssociateAccessPolicy":              false,
		"DisassociateAccessPolicy":           false,
		"ListAssociatedAccessPolicies":       false,
		"ListAccessPolicies":                 false,
		"ListAddons":                         false,
		"DescribeAddonVersions":              false,
		"CreateAddon":                        false,
		"DeleteAddon":                        false,
		"DescribeAddon":                      false,
		"UpdateAddon":                        false,
		"AssociateIdentityProviderConfig":    false,
		"DescribeIdentityProviderConfig":     false,
		"ListIdentityProviderConfigs":        false,
		"DisassociateIdentityProviderConfig": false,
		"TagResource":                        false,
		"UntagResource":                      false,
		"ListTagsForResource":                false,
	}
	for _, route := range eksRoutes {
		if _, ok := expected[route.action]; ok {
			expected[route.action] = true
		}
	}
	for action, seen := range expected {
		assert.True(t, seen, "action %q is not registered in eksRoutes", action)
	}
	assert.Equal(t, len(expected), len(eksRoutes), "eksRoutes should have exactly one entry per AWS action")
}

func TestLookupEKSAction_UnknownReturnsFalse(t *testing.T) {
	_, _, _, ok := lookupEKSAction("PATCH", "/clusters/alpha")
	assert.False(t, ok)

	_, _, _, ok = lookupEKSAction("GET", "/clusters/alpha/wat")
	assert.False(t, ok)
}

func TestEKSRequest_UnknownActionReturnsInvalidAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := httptest.NewRequest(http.MethodGet, "/totally-unknown", nil)
	ctx := context.WithValue(req.Context(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, "111122223333")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	err := gw.EKS_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestEKSRequest_MissingNATSReturnsServerInternal(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	ctx := context.WithValue(req.Context(), ctxService, "eks")
	ctx = context.WithValue(ctx, ctxAccountID, "111122223333")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	err := gw.EKS_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestErrorHandler_EKSEmitsJSONNotXML(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	ctx := context.WithValue(req.Context(), ctxService, "eks")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	gw.ErrorHandler(w, req, errEKSNotImpl())
	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Equal(t, gateway_eks.JSONContentType, w.Header().Get("Content-Type"))
	assert.True(t, strings.HasPrefix(w.Body.String(), "{"), "expected JSON body, got %q", w.Body.String())

	var env gateway_eks.EKSJSONError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "NotImplementedException", env.Type)
}

// errEKSNotImpl returns the error a stub EKS handler would surface.
func errEKSNotImpl() error { return errAWS(awserrors.ErrorNotImplemented) }

// awsCodeError lets tests return an awserrors-code error without depending on the impl package.
type awsCodeError struct{ code string }

func (e *awsCodeError) Error() string { return e.code }
func errAWS(code string) error        { return &awsCodeError{code: code} }
