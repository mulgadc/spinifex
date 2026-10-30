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

// bedrock-agent's real AWS paths carry a trailing slash on the collection
// routes; chi reports those patterns without it, so the entry key trims both.
func TestRESTRouter_TrailingSlashIsExact(t *testing.T) {
	action, _, _, ok := bedrockAgentRouter.Lookup("PUT", "/knowledgebases/")
	require.True(t, ok)
	assert.Equal(t, "CreateKnowledgeBase", action)

	_, _, _, ok = bedrockAgentRouter.Lookup("PUT", "/knowledgebases")
	assert.False(t, ok, "the slashless path is a different route to AWS")
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
	t.Run("bedrock", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockRoutes) })
	t.Run("bedrock-runtime", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockRuntimeRoutes) })
	t.Run("bedrock-agent", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockAgentRoutes) })
	t.Run("bedrock-agent-runtime", func(t *testing.T) { assertNoDuplicateRoutes(t, bedrockAgentRuntimeRoutes) })
}
