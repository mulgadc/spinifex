package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/stretchr/testify/assert"
)

func TestHasPublicIPPools(t *testing.T) {
	transit := config.ExternalPool{Name: host.NATTransitPoolName, Gateway: host.NATTransitGatewayIP, PrefixLen: 24}
	wanStatic := config.ExternalPool{Name: "wan", RangeStart: "192.168.1.150", RangeEnd: "192.168.1.250", Gateway: "192.168.1.1", PrefixLen: 24}
	wanDHCP := config.ExternalPool{Name: "wan", Source: "dhcp", BindBridge: "wlan0"}

	tests := []struct {
		name  string
		mode  string
		pools []config.ExternalPool
		want  bool
	}{
		{name: "pool mode", mode: "pool", pools: []config.ExternalPool{wanStatic}, want: true},
		{name: "nat transit only", mode: "nat", pools: []config.ExternalPool{transit}, want: false},
		{name: "nat with static wan", mode: "nat", pools: []config.ExternalPool{transit, wanStatic}, want: true},
		{name: "nat with dhcp wan", mode: "nat", pools: []config.ExternalPool{transit, wanDHCP}, want: true},
		{name: "nat no pools", mode: "nat", pools: nil, want: false},
		{name: "external disabled", mode: "", pools: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Daemon{clusterConfig: &config.ClusterConfig{}}
			d.clusterConfig.Network.ExternalMode = tt.mode
			d.clusterConfig.Network.ExternalPools = tt.pools
			assert.Equal(t, tt.want, d.hasPublicIPPools())
		})
	}

	t.Run("nil cluster config", func(t *testing.T) {
		d := &Daemon{}
		assert.False(t, d.hasPublicIPPools())
	})
}

func TestPublicExternalPools_FiltersTransit(t *testing.T) {
	pools := []config.ExternalPool{
		{Name: host.NATTransitPoolName},
		{Name: "wan", Source: "dhcp", BindBridge: "wlan0"},
	}
	public := publicExternalPools(pools)
	assert.Len(t, public, 1)
	assert.Equal(t, "wan", public[0].Name)
}
