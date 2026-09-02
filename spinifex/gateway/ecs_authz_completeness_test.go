package gateway_test

import (
	"testing"

	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
	"github.com/stretchr/testify/assert"
)

// TestECSScopeTableIsExhaustive is what stops the next ECS action being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestECSScopeTableIsExhaustive(t *testing.T) {
	for action := range gateway_ecs.Actions {
		assert.True(t, gateway_ecs.HasScope(action),
			"ecs action %q has no resource scope entry: add one to ecsScopes in gateway/ecs/authz.go", action)
	}

	for _, action := range gateway_ecs.ScopedActions() {
		_, ok := gateway_ecs.Actions[action]
		assert.True(t, ok,
			"ecsScopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/ecs/authz.go", action)
	}
}
