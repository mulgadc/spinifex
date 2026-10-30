//test:in-package — ec2Actions is unexported, and the completeness test exists
// to compare it against the scope table.

package awsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEC2ScopeTableIsExhaustive is what stops the next EC2 action being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestEC2ScopeTableIsExhaustive(t *testing.T) {
	for action := range ec2Actions {
		assert.True(t, HasScope(action),
			"ec2 action %q has no resource scope entry: add one to ec2Scopes in domains/ec2/awsapi/authz.go", action)
	}

	for _, action := range ScopedActions() {
		_, ok := ec2Actions[action]
		assert.True(t, ok,
			"ec2Scopes has an entry for %q, which the dispatch table does not serve: remove it from domains/ec2/awsapi/authz.go", action)
	}
}
