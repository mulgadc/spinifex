//test:in-package — drives the unexported pool projection startCluster feeds to external IPAM.
package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func daemonWithPools(pools ...config.ExternalPool) *Daemon {
	d := &Daemon{clusterConfig: &config.ClusterConfig{}}
	d.clusterConfig.Network.ExternalPools = pools
	return d
}

func TestExternalPoolConfigsCopiesEveryField(t *testing.T) {
	d := daemonWithPools(config.ExternalPool{
		Name:            "wan",
		Source:          "static",
		BindBridge:      "br-wan",
		DHCPMAC:         "derived",
		RangeStart:      "192.168.1.150",
		RangeEnd:        "192.168.1.250",
		Gateway:         "192.168.1.1",
		GatewayIP:       "192.168.1.149",
		PrefixLen:       24,
		Region:          "us-west-1",
		AZ:              "us-west-1-az2",
		GwLrpRangeStart: "192.168.1.100",
		GwLrpRangeEnd:   "192.168.1.110",
	})

	pools, anyDHCP := d.externalPoolConfigs()
	require.Len(t, pools, 1)
	assert.False(t, anyDHCP, "a static pool must not ask for the DHCP allocator")

	got := pools[0]
	assert.Equal(t, "wan", got.Name)
	assert.Equal(t, "static", got.Source)
	assert.Equal(t, "br-wan", got.BindBridge)
	assert.Equal(t, "derived", got.DHCPMAC)
	assert.Equal(t, "192.168.1.150", got.RangeStart)
	assert.Equal(t, "192.168.1.250", got.RangeEnd)
	assert.Equal(t, "192.168.1.1", got.Gateway)
	assert.Equal(t, "192.168.1.149", got.GatewayIP)
	assert.Equal(t, 24, got.PrefixLen)
	assert.Equal(t, "us-west-1", got.Region)
	assert.Equal(t, "us-west-1-az2", got.AZ)
	assert.Equal(t, "192.168.1.100", got.GwLrpRangeStart)
	assert.Equal(t, "192.168.1.110", got.GwLrpRangeEnd)
}

// The transit pool's addresses are gateway-LRP plumbing. Handing them to IPAM
// would let a tenant allocate one as an EIP and break the router.
func TestExternalPoolConfigsExcludesTheTransitPool(t *testing.T) {
	d := daemonWithPools(
		config.ExternalPool{Name: host.NATTransitPoolName, Source: "static"},
		config.ExternalPool{Name: "wan", Source: "static"},
	)

	pools, _ := d.externalPoolConfigs()
	require.Len(t, pools, 1)
	assert.Equal(t, "wan", pools[0].Name)
}

func TestExternalPoolConfigsReportsDHCP(t *testing.T) {
	d := daemonWithPools(
		config.ExternalPool{Name: "wan", Source: "static"},
		config.ExternalPool{Name: "wan2", Source: "dhcp", BindBridge: "br-wan"},
	)

	pools, anyDHCP := d.externalPoolConfigs()
	require.Len(t, pools, 2)
	assert.True(t, anyDHCP, "one DHCP pool must enable the allocator for the node")
}

// A transit-only config yields no pools, which is what keeps nat mode off the
// EIP path without a separate mode check at the call site.
func TestExternalPoolConfigsTransitOnlyYieldsNothing(t *testing.T) {
	d := daemonWithPools(config.ExternalPool{Name: host.NATTransitPoolName, Source: "static"})

	pools, anyDHCP := d.externalPoolConfigs()
	assert.Empty(t, pools)
	assert.False(t, anyDHCP)
}
