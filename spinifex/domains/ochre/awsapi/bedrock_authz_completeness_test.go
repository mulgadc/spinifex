//test:in-package — the four Bedrock route tables are unexported here, and the
// completeness test exists to compare them against the scope tables.

package awsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBedrockScopeTablesAreExhaustive is what stops the next Bedrock-family
// route being added with a silent account-wide grant. It asserts both
// directions per service, so a scope left behind by a deleted or renamed action
// fails too.
func TestBedrockScopeTablesAreExhaustive(t *testing.T) {
	served := map[string][]string{
		"bedrock":               actionsOf(bedrockRoutes, func(r bedrockRoute) string { return r.Action }),
		"bedrock-runtime":       actionsOf(bedrockRuntimeRoutes, func(r bedrockRuntimeRoute) string { return r.Action }),
		"bedrock-agent":         actionsOf(bedrockAgentRoutes, func(r bedrockAgentRoute) string { return r.Action }),
		"bedrock-agent-runtime": actionsOf(bedrockAgentRuntimeRoutes, func(r bedrockAgentRuntimeRoute) string { return r.Action }),
	}

	for service, actions := range served {
		names := make(map[string]bool, len(actions))
		for _, action := range actions {
			names[action] = true
			assert.True(t, HasScope(service, action),
				"%s action %q has no resource scope entry: add one to bedrockScopes in domains/ochre/awsapi/authz.go",
				service, action)
		}
		for _, action := range ScopedActions(service) {
			assert.True(t, names[action],
				"bedrockScopes[%q] has an entry for %q, which the dispatch table does not serve: remove it from domains/ochre/awsapi/authz.go",
				service, action)
		}
	}
}
