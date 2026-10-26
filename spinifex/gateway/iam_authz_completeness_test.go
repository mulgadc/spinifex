//test:in-package — iamActions is unexported and must be compared with the scope table.

package gateway

import (
	"testing"

	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	"github.com/stretchr/testify/assert"
)

func TestIAMScopeTableIsExhaustive(t *testing.T) {
	for action := range iamActions {
		assert.True(t, iamawsapi.HasScope(action),
			"IAM action %q has no resource scope entry", action)
	}
	for _, action := range iamawsapi.ScopedActions() {
		_, ok := iamActions[action]
		assert.True(t, ok, "IAM scope table contains unserved action %q", action)
	}
}
