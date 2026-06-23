package handlers_ec2_instance

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- generateNetworkConfig ---

func TestGenerateNetworkConfig_BothEmpty(t *testing.T) {
	cfg := generateNetworkConfig("", "", "", "", nil)
	assert.Equal(t, cloudInitNetworkConfigWildcard, cfg)
}

func TestGenerateNetworkConfig_OneEmpty(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "", "", "", nil)
	assert.Contains(t, cfg, "vpc0:", "eniMAC alone should produce per-interface config")
	assert.NotContains(t, cfg, "dev0:", "no dev NIC without devMAC")

	cfg = generateNetworkConfig("", "02:00:00:dd:ee:ff", "", "", nil)
	assert.Equal(t, cloudInitNetworkConfigWildcard, cfg, "should fall back to wildcard if eniMAC empty")
}

func TestGenerateNetworkConfig_DualNIC(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "", nil)
	assert.Contains(t, cfg, "version: 2")
	assert.Contains(t, cfg, `macaddress: "02:00:00:aa:bb:cc"`)
	assert.Contains(t, cfg, `macaddress: "02:00:00:dd:ee:ff"`)
	assert.Contains(t, cfg, "use-routes: false")
	assert.Contains(t, cfg, "use-dns: false")
	assert.Contains(t, cfg, "vpc0:")
	assert.Contains(t, cfg, "dev0:")
	assert.NotContains(t, cfg, "mgmt0:")
}

func TestGenerateNetworkConfig_TripleNIC(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:de:00:dd:ee:ff", "02:a0:00:11:22:33", "10.15.8.101", nil)
	assert.Contains(t, cfg, "version: 2")
	assert.Contains(t, cfg, `macaddress: "02:00:00:aa:bb:cc"`)
	assert.Contains(t, cfg, `macaddress: "02:de:00:dd:ee:ff"`)
	assert.Contains(t, cfg, "mgmt0:")
	assert.Contains(t, cfg, `macaddress: "02:a0:00:11:22:33"`)
	assert.Contains(t, cfg, `"10.15.8.101/24"`)
	// mgmt NIC should not have DHCP or routes
	assert.Contains(t, cfg, "vpc0:")
	assert.Contains(t, cfg, "dev0:")
}

func TestGenerateNetworkConfig_MgmtWithoutDev(t *testing.T) {
	// System instances: eniMAC + mgmtMAC, no devMAC — should get per-interface config with mgmt NIC
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "", "02:a0:00:11:22:33", "10.15.8.101", nil)
	assert.Contains(t, cfg, "vpc0:")
	assert.NotContains(t, cfg, "dev0:", "no dev NIC without devMAC")
	assert.Contains(t, cfg, "mgmt0:")
	assert.Contains(t, cfg, `macaddress: "02:a0:00:11:22:33"`)
	assert.Contains(t, cfg, `"10.15.8.101/24"`)
}

func TestGenerateNetworkConfig_MgmtMACWithoutIP(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:de:00:dd:ee:ff", "02:a0:00:11:22:33", "", nil)
	assert.NotContains(t, cfg, "mgmt0:", "mgmt NIC should not appear without IP")
}

func TestGenerateNetworkConfig_MgmtIPWithoutMAC(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:de:00:dd:ee:ff", "", "10.15.8.101", nil)
	assert.NotContains(t, cfg, "mgmt0:", "mgmt NIC should not appear without MAC")
}

// The per-tap IMDS datapath intercepts 169.254.169.254 at the host tap, so guests
// no longer need an in-guest on-link route — the network-config must not emit one
// on any NIC (primary, extra, or dev).
func TestGenerateNetworkConfig_NoIMDSRoute(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "",
		[]string{"02:00:00:bb:bb:bb"})
	assert.Contains(t, cfg, "vpc0:")
	assert.NotContains(t, cfg, "169.254.169.254", "no in-guest IMDS route (served at the tap)")
}

// No family carries the in-guest IMDS route now that it is served at the tap.
// RHEL renders its NICs via NM keyfiles (config disabled here), so it is covered
// in TestBuildRHELCloudInit instead.
func TestSelectNetworkConfigForFamily_NoIMDSRoute(t *testing.T) {
	for _, family := range []string{"debian", "ubuntu", "alpine"} {
		cfg := selectNetworkConfigForFamily(family, "02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "", nil)
		assert.Contains(t, cfg, "vpc0:", family)
		assert.NotContains(t, cfg, "169.254.169.254", family+" must not carry the IMDS route")
	}
}

// Route for multi-node LB mgmt traffic is handled via the fw_cfg netcfg
// blob delivered to the LB microVM, not in the network-config.
