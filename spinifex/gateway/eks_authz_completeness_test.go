//test:in-package — eksRoutes is unexported here, and the completeness test
// exists to compare it against the scope table.

package gateway

import (
	"testing"

	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	"github.com/stretchr/testify/assert"
)

// TestEKSScopeTableIsExhaustive is what stops the next EKS route being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestEKSScopeTableIsExhaustive(t *testing.T) {
	served := make(map[string]bool, len(eksRoutes))
	for _, route := range eksRoutes {
		served[route.Action] = true
		assert.True(t, eksawsapi.HasScope(route.Action),
			"eks action %q has no resource scope entry: add one to eksScopes in domains/eks/awsapi/authz.go", route.Action)
	}

	for _, action := range eksawsapi.ScopedActions() {
		assert.True(t, served[action],
			"eksScopes has an entry for %q, which the dispatch table does not serve: remove it from domains/eks/awsapi/authz.go", action)
	}
}
