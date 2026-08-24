package gateway

import (
	"testing"

	gateway_ec2 "github.com/mulgadc/spinifex/spinifex/gateway/ec2"
	"github.com/stretchr/testify/assert"
)

// TestEC2ScopeTableIsExhaustive is what stops the next EC2 action being added
// with a silent account-wide grant. It asserts both directions, so a scope left
// behind by a deleted or renamed action fails too.
func TestEC2ScopeTableIsExhaustive(t *testing.T) {
	for action := range ec2Actions {
		assert.True(t, gateway_ec2.HasScope(action),
			"ec2 action %q has no resource scope entry: add one to ec2Scopes in gateway/ec2/authz.go", action)
	}

	for _, action := range gateway_ec2.ScopedActions() {
		_, ok := ec2Actions[action]
		assert.True(t, ok,
			"ec2Scopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/ec2/authz.go", action)
	}
}
