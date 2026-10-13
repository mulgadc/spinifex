//test:in-package — taggingActions is unexported here, and the completeness test
// exists to compare it against the scope table.

package gateway

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	"github.com/stretchr/testify/assert"
)

// TestTaggingScopeTableIsExhaustive is what stops the next tagging action being
// added with a silent account-wide grant. It asserts both directions, so a scope
// left behind by a deleted or renamed action fails too.
func TestTaggingScopeTableIsExhaustive(t *testing.T) {
	for action := range taggingActions {
		assert.True(t, tagging.HasScope(action),
			"tagging action %q has no resource scope entry: add one to taggingScopes in domains/tagging/authz.go", action)
	}

	for _, action := range tagging.ScopedActions() {
		_, ok := taggingActions[action]
		assert.True(t, ok,
			"taggingScopes has an entry for %q, which the dispatch table does not serve: remove it from domains/tagging/authz.go", action)
	}
}
