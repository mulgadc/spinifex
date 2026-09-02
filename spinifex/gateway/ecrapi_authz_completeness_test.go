//test:in-package — ecrInlineActions is unexported here, and ECR dispatch is the
// union of it and the ecrapi namespace.

package gateway

import (
	"testing"

	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	"github.com/stretchr/testify/assert"
)

// TestECRScopeTableIsExhaustive is what stops the next ECR action being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestECRScopeTableIsExhaustive(t *testing.T) {
	for action := range gateway_ecrapi.Actions {
		assert.True(t, gateway_ecrapi.HasScope(action),
			"ecr action %q has no resource scope entry: add one to ecrScopes in gateway/ecrapi/authz.go", action)
	}

	for _, action := range gateway_ecrapi.ScopedActions() {
		_, ok := gateway_ecrapi.Actions[action]
		assert.True(t, ok,
			"ecrScopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/ecrapi/authz.go", action)
	}
}

// ECR_Request resolves the action against the ecrapi namespace before it reaches
// the inline table, so an inline handler outside that namespace is unreachable —
// and, more to the point, would never have had its resources scoped.
func TestECRInlineActionsAreInTheNamespace(t *testing.T) {
	for action := range ecrInlineActions {
		_, ok := gateway_ecrapi.Actions[action]
		assert.True(t, ok,
			"inline ECR handler %q is not in gateway_ecrapi.Actions, so ECR_Request rejects it as InvalidAction", action)
	}
}
