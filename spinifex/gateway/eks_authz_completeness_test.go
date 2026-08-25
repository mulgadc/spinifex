//test:in-package — eksRoutes is unexported here, and the completeness test
// exists to compare it against the scope table.

package gateway

import (
	"testing"

	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
	"github.com/stretchr/testify/assert"
)

// TestEKSScopeTableIsExhaustive is what stops the next EKS route being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestEKSScopeTableIsExhaustive(t *testing.T) {
	served := make(map[string]bool, len(eksRoutes))
	for _, route := range eksRoutes {
		served[route.action] = true
		assert.True(t, gateway_eks.HasScope(route.action),
			"eks action %q has no resource scope entry: add one to eksScopes in gateway/eks/authz.go", route.action)
	}

	for _, action := range gateway_eks.ScopedActions() {
		assert.True(t, served[action],
			"eksScopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/eks/authz.go", action)
	}
}
