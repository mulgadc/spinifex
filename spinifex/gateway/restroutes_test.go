//test:in-package — the route tables are unexported, and these tests hold each
// service table to what the REST router resolves.

package gateway

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// samplePath fills a chi pattern's params with placeholder segments, keeping
// every literal segment (and any trailing slash) intact.
func samplePath(pattern string) string {
	segments := strings.Split(pattern, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") || segment == "*" {
			segments[i] = "sample"
		}
	}
	return strings.Join(segments, "/")
}

// assertRoutesResolve walks a table and checks each route is reachable through
// the router, guarding against a pattern the trie canonicalises away from its
// entry key — which lookup would otherwise report as a plain no-match.
func assertRoutesResolve[H any](t *testing.T, rr *rest.Router[H], routes []rest.Route[H]) {
	t.Helper()
	for _, route := range routes {
		path := samplePath(route.Pattern)
		action, params, _, ok := rr.Lookup(route.Method, path)
		require.True(t, ok, "%s %s (pattern %s) should match", route.Method, path, route.Pattern)
		assert.Equal(t, route.Action, action, "%s %s", route.Method, path)
		assert.Len(t, params, strings.Count(route.Pattern, "{")+strings.Count(route.Pattern, "*"), "%s %s", route.Method, path)
	}
}

func TestRESTRouters_EveryRouteResolves(t *testing.T) {
	t.Run("eks", func(t *testing.T) { assertRoutesResolve(t, eksRouter, eksRoutes) })
	t.Run("bedrock", func(t *testing.T) { assertRoutesResolve(t, bedrockRouter, bedrockRoutes) })
	t.Run("bedrock-runtime", func(t *testing.T) {
		assertRoutesResolve(t, bedrockRuntimeRouter, bedrockRuntimeRoutes)
	})
	t.Run("bedrock-agent", func(t *testing.T) {
		assertRoutesResolve(t, bedrockAgentRouter, bedrockAgentRoutes)
	})
	t.Run("bedrock-agent-runtime", func(t *testing.T) {
		assertRoutesResolve(t, bedrockAgentRuntimeRouter, bedrockAgentRuntimeRoutes)
	})
}

// The regex tables anchored every pattern, so a path had to match end to end.
func TestRESTRouter_NoMatchCases(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"empty param segment", "GET", "/clusters/"},
		{"empty wildcard", "GET", "/tags/"},
		{"unregistered method", "PATCH", "/clusters/alpha"},
		{"trailing slash on a slashless pattern", "GET", "/clusters/alpha/"},
		{"unknown suffix", "GET", "/clusters/alpha/wat"},
		{"wrong method on a deep route", "GET", "/clusters/alpha/node-groups/ng/update-config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, params, _, ok := eksRouter.Lookup(tc.method, tc.path)
			assert.False(t, ok)
			assert.Empty(t, action)
			assert.Nil(t, params)
		})
	}
}

// bedrock-agent's real AWS paths carry a trailing slash on the collection
// routes; chi reports those patterns without it, so the entry key trims both.
func TestRESTRouter_TrailingSlashIsExact(t *testing.T) {
	action, _, _, ok := bedrockAgentRouter.Lookup("PUT", "/knowledgebases/")
	require.True(t, ok)
	assert.Equal(t, "CreateKnowledgeBase", action)

	_, _, _, ok = bedrockAgentRouter.Lookup("PUT", "/knowledgebases")
	assert.False(t, ok, "the slashless path is a different route to AWS")
}

// Params arrive percent-encoded so an ARN stays one path segment; the wildcard
// route keeps the slashes the encoding hides.
func TestRESTRouter_UnescapesParams(t *testing.T) {
	const (
		arn     = "arn:aws:iam::000000000001:user/admin"
		escaped = "arn%3Aaws%3Aiam%3A%3A000000000001%3Auser%2Fadmin"
	)

	action, params, _, ok := eksRouter.Lookup("GET", "/clusters/alpha/access-entries/"+escaped)
	require.True(t, ok)
	assert.Equal(t, "DescribeAccessEntry", action)
	assert.Equal(t, []string{"alpha", arn}, params)

	action, params, _, ok = eksRouter.Lookup("GET", "/tags/"+escaped)
	require.True(t, ok)
	assert.Equal(t, "ListTagsForResource", action)
	assert.Equal(t, []string{arn}, params)
}

// chi keeps the last registration of a duplicated method+pattern silently,
// leaving the shadowed route's action unreachable.
func assertNoDuplicateRoutes[H any](t *testing.T, routes []rest.Route[H]) {
	t.Helper()
	seen := make(map[string]string, len(routes))
	for _, route := range routes {
		key := route.Method + " " + strings.TrimSuffix(route.Pattern, "/")
		assert.NotContains(t, seen, key, "%s shadows %s on %s", route.Action, seen[key], key)
		seen[key] = route.Action
	}
}

func TestRESTRouters_NoDuplicateRoutes(t *testing.T) {
	t.Run("eks", func(t *testing.T) { assertNoDuplicateRoutes(t, eksRoutes) })
	t.Run("bedrock", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockRoutes) })
	t.Run("bedrock-runtime", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockRuntimeRoutes) })
	t.Run("bedrock-agent", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockAgentRoutes) })
	t.Run("bedrock-agent-runtime", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockAgentRuntimeRoutes) })
}
