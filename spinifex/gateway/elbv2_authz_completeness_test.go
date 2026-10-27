//test:in-package — elbv2Actions is unexported here, and the completeness test
// exists to compare it against the scope table.

package gateway

import (
	"testing"

	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	"github.com/stretchr/testify/assert"
)

// TestELBv2ScopeTableIsExhaustive is what stops the next ELBv2 action being
// added with a silent account-wide grant. It asserts both directions, so a
// scope left behind by a deleted or renamed action fails too.
func TestELBv2ScopeTableIsExhaustive(t *testing.T) {
	for action := range elbv2Actions {
		assert.True(t, elbv2awsapi.HasScope(action),
			"elbv2 action %q has no resource scope entry: add one to elbv2Scopes in domains/elbv2/awsapi/authz.go", action)
	}

	for _, action := range elbv2awsapi.ScopedActions() {
		_, ok := elbv2Actions[action]
		assert.True(t, ok,
			"elbv2Scopes has an entry for %q, which the dispatch table does not serve: remove it from domains/elbv2/awsapi/authz.go", action)
	}
}
