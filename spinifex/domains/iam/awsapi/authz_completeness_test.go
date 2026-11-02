package awsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIAMScopeTableIsExhaustive(t *testing.T) {
	for action := range actions {
		assert.True(t, HasScope(action),
			"IAM action %q has no resource scope entry", action)
	}
	for _, action := range ScopedActions() {
		_, ok := actions[action]
		assert.True(t, ok, "IAM scope table contains unserved action %q", action)
	}
}
