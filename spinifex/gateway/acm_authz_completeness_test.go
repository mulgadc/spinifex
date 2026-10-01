//test:in-package — acmActions is unexported here, and the completeness test
// exists to compare it against the scope table.

package gateway

import (
	"testing"

	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/stretchr/testify/assert"
)

// TestACMScopeTableIsExhaustive is what stops the next ACM action being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestACMScopeTableIsExhaustive(t *testing.T) {
	for action := range acmActions {
		assert.True(t, acmawsapi.HasScope(action),
			"acm action %q has no resource scope entry: add one to domains/acm/awsapi/authz.go", action)
	}

	for _, action := range acmawsapi.ScopedActions() {
		_, ok := acmActions[action]
		assert.True(t, ok,
			"acmScopes has an entry for %q, which the dispatch table does not serve: remove it from domains/acm/awsapi/authz.go", action)
	}
}
