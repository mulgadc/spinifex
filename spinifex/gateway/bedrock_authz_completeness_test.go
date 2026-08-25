//test:in-package — the four Bedrock route tables are unexported here, and the
// completeness test exists to compare them against the scope tables.

package gateway

import (
	"testing"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/stretchr/testify/assert"
)

// TestBedrockScopeTablesAreExhaustive is what stops the next Bedrock-family
// route being added with a silent account-wide grant. It asserts both
// directions per service, so a scope left behind by a deleted or renamed action
// fails too.
func TestBedrockScopeTablesAreExhaustive(t *testing.T) {
	served := map[string][]string{
		"bedrock":               actionsOf(bedrockRoutes, func(r bedrockRoute) string { return r.action }),
		"bedrock-runtime":       actionsOf(bedrockRuntimeRoutes, func(r bedrockRuntimeRoute) string { return r.action }),
		"bedrock-agent":         actionsOf(bedrockAgentRoutes, func(r bedrockAgentRoute) string { return r.action }),
		"bedrock-agent-runtime": actionsOf(bedrockAgentRuntimeRoutes, func(r bedrockAgentRuntimeRoute) string { return r.action }),
	}

	for service, actions := range served {
		names := make(map[string]bool, len(actions))
		for _, action := range actions {
			names[action] = true
			assert.True(t, gateway_bedrock.HasScope(service, action),
				"%s action %q has no resource scope entry: add one to bedrockScopes in gateway/bedrock/authz.go",
				service, action)
		}
		for _, action := range gateway_bedrock.ScopedActions(service) {
			assert.True(t, names[action],
				"bedrockScopes[%q] has an entry for %q, which the dispatch table does not serve: remove it from gateway/bedrock/authz.go",
				service, action)
		}
	}
}

func actionsOf[T any](routes []T, action func(T) string) []string {
	actions := make([]string, 0, len(routes))
	for _, route := range routes {
		actions = append(actions, action(route))
	}
	return actions
}
