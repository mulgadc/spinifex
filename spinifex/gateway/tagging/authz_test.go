package gateway_tagging_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_tagging "github.com/mulgadc/spinifex/spinifex/gateway/tagging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Resource Groups Tagging API defines no resource types, so every action it
// serves is evaluated against "*". Scoping GetResources to the resources it
// happens to return would deny calls AWS permits.
func TestResourceARNsAreAccountLevel(t *testing.T) {
	for _, action := range gateway_tagging.ScopedActions() {
		resources, err := gateway_tagging.ResourceARNs(action)
		require.NoError(t, err, "action %q", action)
		assert.Equal(t, []string{"*"}, resources, "action %q", action)
	}
}

// An action absent from the dispatch table cannot reach the resolver, but if one
// ever did it fails closed rather than authorizing account-wide.
func TestResourceARNsUnknownAction(t *testing.T) {
	_, err := gateway_tagging.ResourceARNs("BogusAction")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestHasScope(t *testing.T) {
	assert.True(t, gateway_tagging.HasScope("GetResources"))
	assert.False(t, gateway_tagging.HasScope("TagResources"))
	assert.Equal(t, []string{"GetResources"}, gateway_tagging.ScopedActions())
}
