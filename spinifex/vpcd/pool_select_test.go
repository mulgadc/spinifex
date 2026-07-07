package vpcd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/host"
)

func TestSelectExternalPools(t *testing.T) {
	transit := external.ExternalPoolConfig{Name: host.NATTransitPoolName, Gateway: host.NATTransitGatewayIP}
	wan := external.ExternalPoolConfig{Name: "wan", Gateway: "192.168.1.1"}

	t.Run("nat picks transit by name regardless of order", func(t *testing.T) {
		igw, public := selectExternalPools("nat", []external.ExternalPoolConfig{wan, transit})
		require.NotNil(t, igw)
		require.NotNil(t, public)
		assert.Equal(t, host.NATTransitPoolName, igw.Name)
		assert.Equal(t, "wan", public.Name)
	})

	t.Run("nat transit only", func(t *testing.T) {
		igw, public := selectExternalPools("nat", []external.ExternalPoolConfig{transit})
		require.NotNil(t, igw)
		assert.Equal(t, host.NATTransitPoolName, igw.Name)
		assert.Nil(t, public)
	})

	t.Run("pool mode keeps first pool, no public split", func(t *testing.T) {
		igw, public := selectExternalPools("pool", []external.ExternalPoolConfig{wan})
		require.NotNil(t, igw)
		assert.Equal(t, "wan", igw.Name)
		assert.Nil(t, public)
	})

	t.Run("no pools", func(t *testing.T) {
		igw, public := selectExternalPools("", nil)
		assert.Nil(t, igw)
		assert.Nil(t, public)
	})
}
