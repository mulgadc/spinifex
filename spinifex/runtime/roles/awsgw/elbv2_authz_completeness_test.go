//test:in-package — the served-action list is a gateway test helper, and the
// completeness test exists to compare it against the scope table.

package awsgw_test

import (
	"testing"

	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	"github.com/stretchr/testify/assert"
)

// TestELBv2ScopeTableIsExhaustive is what stops the next ELBv2 action being
// added with a silent account-wide grant. It asserts both directions, so a
// scope left behind by a deleted or renamed action fails too.
func TestELBv2ScopeTableIsExhaustive(t *testing.T) {
	served := elbv2ServedActions()
	for _, action := range served {
		assert.True(t, elbv2awsapi.HasScope(action),
			"elbv2 action %q has no resource scope entry: add one to elbv2Scopes in domains/elbv2/awsapi/authz.go", action)
	}

	for _, action := range elbv2awsapi.ScopedActions() {
		assert.Contains(t, served, action,
			"elbv2Scopes has an entry for %q, which the dispatch table does not serve: remove it from domains/elbv2/awsapi/authz.go", action)
	}
}
