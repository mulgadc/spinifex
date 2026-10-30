//test:in-package — routes is unexported here, and the completeness test
// exists to compare it against the scope table.

package awsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEKSScopeTableIsExhaustive is what stops the next EKS route being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestEKSScopeTableIsExhaustive(t *testing.T) {
	served := make(map[string]bool, len(routes))
	for _, route := range routes {
		served[route.Action] = true
		assert.True(t, HasScope(route.Action),
			"eks action %q has no resource scope entry: add one to eksScopes in domains/eks/awsapi/authz.go", route.Action)
	}

	for _, action := range ScopedActions() {
		assert.True(t, served[action],
			"eksScopes has an entry for %q, which the dispatch table does not serve: remove it from domains/eks/awsapi/authz.go", action)
	}
}
