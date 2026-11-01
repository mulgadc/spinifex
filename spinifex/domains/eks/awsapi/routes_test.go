package awsapi

import (
	"slices"
	"strings"
	"testing"

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
		{"POST", "/clusters/alpha/node-groups", "CreateNodegroup", []string{"alpha"}},
		{"GET", "/clusters/alpha/node-groups", "ListNodegroups", []string{"alpha"}},
		{"GET", "/clusters/alpha/node-groups/ng1", "DescribeNodegroup", []string{"alpha", "ng1"}},
		{"DELETE", "/clusters/alpha/node-groups/ng1", "DeleteNodegroup", []string{"alpha", "ng1"}},
		{"POST", "/clusters/alpha/node-groups/ng1/update-config", "UpdateNodegroupConfig", []string{"alpha", "ng1"}},
		{"POST", "/clusters/alpha/access-entries", "CreateAccessEntry", []string{"alpha"}},
		{"GET", "/clusters/alpha/access-entries", "ListAccessEntries", []string{"alpha"}},
		{"GET", "/clusters/alpha/access-entries/arn-user", "DescribeAccessEntry", []string{"alpha", "arn-user"}},
		{"POST", "/clusters/alpha/access-entries/arn-user", "UpdateAccessEntry", []string{"alpha", "arn-user"}},
		{"DELETE", "/clusters/alpha/access-entries/arn-user", "DeleteAccessEntry", []string{"alpha", "arn-user"}},
		{"POST", "/clusters/alpha/access-entries/arn-user/access-policies", "AssociateAccessPolicy", []string{"alpha", "arn-user"}},
		{"DELETE", "/clusters/alpha/access-entries/arn-user/access-policies/policy-arn", "DisassociateAccessPolicy", []string{"alpha", "arn-user", "policy-arn"}},
		{"GET", "/clusters/alpha/access-entries/arn-user/access-policies", "ListAssociatedAccessPolicies", []string{"alpha", "arn-user"}},
		{"GET", "/access-policies", "ListAccessPolicies", nil},
		{"GET", "/clusters/alpha/addons", "ListAddons", []string{"alpha"}},
		{"GET", "/addons/supported-versions", "DescribeAddonVersions", nil},
		{"POST", "/clusters/alpha/addons", "CreateAddon", []string{"alpha"}},
		{"GET", "/clusters/alpha/addons/vpc-cni", "DescribeAddon", []string{"alpha", "vpc-cni"}},
		{"DELETE", "/clusters/alpha/addons/vpc-cni", "DeleteAddon", []string{"alpha", "vpc-cni"}},
		{"POST", "/clusters/alpha/addons/vpc-cni/update", "UpdateAddon", []string{"alpha", "vpc-cni"}},
		{"POST", "/tags/arn-cluster", "TagResource", []string{"arn-cluster"}},
		{"DELETE", "/tags/arn-cluster", "UntagResource", []string{"arn-cluster"}},
		{"GET", "/tags/arn-cluster", "ListTagsForResource", []string{"arn-cluster"}},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			action, params, handler, ok := router.Lookup(tc.method, tc.path)
			require.True(t, ok, "expected route to match for %s %s", tc.method, tc.path)
			require.NotNil(t, handler)
			assert.Equal(t, tc.wantAction, action)
			assert.Equal(t, tc.wantParams, params)
		})
	}
}

// IAM principal ARNs are percent-encoded in the request path; router.lookup is
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
		{"GET", "/clusters/alpha/access-entries/" + escaped + "/access-policies", "ListAssociatedAccessPolicies"},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.wantAction, func(t *testing.T) {
			action, params, handler, ok := router.Lookup(tc.method, tc.path)
			require.True(t, ok, "encoded ARN path should match: %s %s", tc.method, tc.path)
			require.NotNil(t, handler)
			assert.Equal(t, tc.wantAction, action)
			assert.Equal(t, []string{"alpha", arn}, params)
		})
	}
}

// DisassociateAccessPolicy carries the policy ARN as the final path segment
// (DELETE .../access-policies/{policyArn}); both principal and policy ARNs are
// percent-encoded and must survive as separate single segments.
func TestLookupEKSAction_DisassociateEncodedARNs(t *testing.T) {
	const (
		principal        = "arn:aws:iam::000000000001:user/admin"
		principalEscaped = "arn%3Aaws%3Aiam%3A%3A000000000001%3Auser%2Fadmin"
		policy           = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
		policyEscaped    = "arn%3Aaws%3Aeks%3A%3Aaws%3Acluster-access-policy%2FAmazonEKSViewPolicy"
	)
	path := "/clusters/alpha/access-entries/" + principalEscaped + "/access-policies/" + policyEscaped
	action, params, handler, ok := router.Lookup("DELETE", path)
	require.True(t, ok, "encoded disassociate path should match: %s", path)
	require.NotNil(t, handler)
	assert.Equal(t, "DisassociateAccessPolicy", action)
	assert.Equal(t, []string{"alpha", principal, policy}, params)
}

func TestLookupEKSAction_CoversAllActions(t *testing.T) {
	expected := map[string]bool{
		"CreateCluster": false,
		// Internal control-plane broker routes, not AWS-SDK EKS actions.
		"PublishInternal":              false,
		"WebhookTokenReview":           false,
		"ListInternalAddons":           false,
		"GetRecoveryDirective":         false,
		"ListInternalWorkers":          false,
		"DescribeCluster":              false,
		"ListClusters":                 false,
		"DeleteCluster":                false,
		"CreateNodegroup":              false,
		"DescribeNodegroup":            false,
		"ListNodegroups":               false,
		"UpdateNodegroupConfig":        false,
		"DeleteNodegroup":              false,
		"CreateAccessEntry":            false,
		"DescribeAccessEntry":          false,
		"ListAccessEntries":            false,
		"UpdateAccessEntry":            false,
		"DeleteAccessEntry":            false,
		"AssociateAccessPolicy":        false,
		"DisassociateAccessPolicy":     false,
		"ListAssociatedAccessPolicies": false,
		"ListAccessPolicies":           false,
		"ListAddons":                   false,
		"DescribeAddonVersions":        false,
		"CreateAddon":                  false,
		"DeleteAddon":                  false,
		"DescribeAddon":                false,
		"UpdateAddon":                  false,
		"TagResource":                  false,
		"UntagResource":                false,
		"ListTagsForResource":          false,
	}
	for _, route := range routes {
		if _, ok := expected[route.Action]; ok {
			expected[route.Action] = true
		}
	}
	for action, seen := range expected {
		assert.True(t, seen, "action %q is not registered in routes", action)
	}
	assert.Len(t, routes, len(expected), "routes should have exactly one entry per AWS action")
}

func TestLookupEKSAction_UnknownReturnsFalse(t *testing.T) {
	_, _, _, ok := router.Lookup("PATCH", "/clusters/alpha")
	assert.False(t, ok)

	_, _, _, ok = router.Lookup("GET", "/clusters/alpha/wat")
	assert.False(t, ok)
}

func TestRESTRouter_EveryEKSRouteResolves(t *testing.T) {
	for _, route := range routes {
		path := samplePath(route.Pattern)
		action, params, _, ok := router.Lookup(route.Method, path)
		require.True(t, ok, "%s %s (pattern %s) should match", route.Method, path, route.Pattern)
		assert.Equal(t, route.Action, action, "%s %s", route.Method, path)
		assert.Len(t, params, strings.Count(route.Pattern, "{")+strings.Count(route.Pattern, "*"), "%s %s", route.Method, path)
	}
}

// samplePath fills a chi pattern's params with placeholder segments.
func samplePath(pattern string) string {
	segments := strings.Split(pattern, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") || segment == "*" {
			segments[i] = "sample"
		}
	}
	return strings.Join(segments, "/")
}

func TestRESTRouter_EKSNoMatchCases(t *testing.T) {
	cases := []struct{ name, method, path string }{
		{"empty param segment", "GET", "/clusters/"},
		{"empty wildcard", "GET", "/tags/"},
		{"unregistered method", "PATCH", "/clusters/alpha"},
		{"trailing slash on a slashless pattern", "GET", "/clusters/alpha/"},
		{"unknown suffix", "GET", "/clusters/alpha/wat"},
		{"wrong method on a deep route", "GET", "/clusters/alpha/node-groups/ng/update-config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, params, _, ok := router.Lookup(tc.method, tc.path)
			assert.False(t, ok)
			assert.Empty(t, action)
			assert.Nil(t, params)
		})
	}
}

// Params arrive percent-encoded so an ARN stays one path segment; the wildcard
// route keeps the slashes the encoding hides.
func TestRESTRouter_EKSUnescapesParams(t *testing.T) {
	const (
		arn     = "arn:aws:iam::000000000001:user/admin"
		escaped = "arn%3Aaws%3Aiam%3A%3A000000000001%3Auser%2Fadmin"
	)

	action, params, _, ok := router.Lookup("GET", "/clusters/alpha/access-entries/"+escaped)
	require.True(t, ok)
	assert.Equal(t, "DescribeAccessEntry", action)
	assert.Equal(t, []string{"alpha", arn}, params)

	action, params, _, ok = router.Lookup("GET", "/tags/"+escaped)
	require.True(t, ok)
	assert.Equal(t, "ListTagsForResource", action)
	assert.Equal(t, []string{arn}, params)
}

// chi keeps the last registration of a duplicated method+pattern silently,
// leaving the shadowed route's action unreachable.
func TestRESTRouter_NoDuplicateEKSRoutes(t *testing.T) {
	seen := make(map[string]string, len(routes))
	for _, route := range routes {
		key := route.Method + " " + strings.TrimSuffix(route.Pattern, "/")
		assert.NotContains(t, seen, key, "%s shadows %s on %s", route.Action, seen[key], key)
		seen[key] = route.Action
	}
}

func TestActionNamesAreSortedAndDistinct(t *testing.T) {
	names := actionNames()

	require.True(t, slices.IsSorted(names), "actions are not sorted: %v", names)
	require.Len(t, slices.Compact(slices.Clone(names)), len(names), "actions contain duplicates: %v", names)
}
