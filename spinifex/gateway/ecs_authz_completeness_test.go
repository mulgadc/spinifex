package gateway_test

import (
	"testing"

	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	"github.com/stretchr/testify/assert"
)

// TestECSScopeTableIsExhaustive is what stops the next ECS action being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestECSScopeTableIsExhaustive(t *testing.T) {
	for action := range ecsawsapi.Actions {
		assert.True(t, ecsawsapi.HasScope(action),
			"ecs action %q has no resource scope entry: add one to ecsScopes in domains/ecs/awsapi/authz.go", action)
	}

	for _, action := range ecsawsapi.ScopedActions() {
		_, ok := ecsawsapi.Actions[action]
		assert.True(t, ok,
			"ecsScopes has an entry for %q, which the dispatch table does not serve: remove it from domains/ecs/awsapi/authz.go", action)
	}
}
