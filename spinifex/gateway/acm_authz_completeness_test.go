//test:in-package — acmActions is unexported here, and the completeness test
// exists to compare it against the scope table.

package gateway

import (
	"testing"

	gateway_acm "github.com/mulgadc/spinifex/spinifex/gateway/acm"
	"github.com/stretchr/testify/assert"
)

// TestACMScopeTableIsExhaustive is what stops the next ACM action being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestACMScopeTableIsExhaustive(t *testing.T) {
	for action := range acmActions {
		assert.True(t, gateway_acm.HasScope(action),
			"acm action %q has no resource scope entry: add one to acmScopes in gateway/acm/authz.go", action)
	}

	for _, action := range gateway_acm.ScopedActions() {
		_, ok := acmActions[action]
		assert.True(t, ok,
			"acmScopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/acm/authz.go", action)
	}
}
