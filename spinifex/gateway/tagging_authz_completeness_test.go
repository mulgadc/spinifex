//test:in-package — taggingActions is unexported here, and the completeness test
// exists to compare it against the scope table.

package gateway

import (
	"testing"

	gateway_tagging "github.com/mulgadc/spinifex/spinifex/gateway/tagging"
	"github.com/stretchr/testify/assert"
)

// TestTaggingScopeTableIsExhaustive is what stops the next tagging action being
// added with a silent account-wide grant. It asserts both directions, so a scope
// left behind by a deleted or renamed action fails too.
func TestTaggingScopeTableIsExhaustive(t *testing.T) {
	for action := range taggingActions {
		assert.True(t, gateway_tagging.HasScope(action),
			"tagging action %q has no resource scope entry: add one to taggingScopes in gateway/tagging/authz.go", action)
	}

	for _, action := range gateway_tagging.ScopedActions() {
		_, ok := taggingActions[action]
		assert.True(t, ok,
			"taggingScopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/tagging/authz.go", action)
	}
}
