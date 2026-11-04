package awsapi

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistration_DeclaresTheEC2Service(t *testing.T) {
	reg := NewRegistration(Deps{})

	assert.Equal(t, ServiceName, reg.Service)
	require.NotNil(t, reg.ResolveAction)
	require.NotNil(t, reg.Dispatch)
	assert.NotNil(t, reg.Errors)
	assert.ElementsMatch(t, ActionNames(), reg.Inventory.Registered)
	assert.Empty(t, reg.Inventory.Stubbed)
	assert.Empty(t, reg.Inventory.Unsupported)

	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(reg))
}
