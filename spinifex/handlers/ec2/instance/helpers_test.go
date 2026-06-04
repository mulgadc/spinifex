package handlers_ec2_instance

import (
	"strings"
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
	// The IMDS on-link route rides vpc0 even with no dev NIC / extra ENIs — the
	// common production shape — so guard it here too, not only on the multi-NIC path.
	assert.Contains(t, cfg, "to: 169.254.169.254/32", "vpc0 must carry the IMDS on-link route")

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

func TestGenerateNetworkConfig_IMDSOnLinkRoute(t *testing.T) {
	// The primary VPC NIC carries an on-link route to the IMDS metadata IP so the
	// guest ARPs 169.254.169.254 directly; the per-subnet localport answers.
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "",
		[]string{"02:00:00:bb:bb:bb"})
	assert.Contains(t, cfg, "to: 169.254.169.254/32")
	assert.Contains(t, cfg, "scope: link")
	// Only the primary NIC (vpc0) gets it — not extra VPC NICs or the dev NIC,
	// matching AWS (IMDS reached via the primary ENI).
	assert.Equal(t, 1, strings.Count(cfg, "169.254.169.254/32"))
}

// Route for multi-node LB mgmt traffic is handled via the fw_cfg netcfg
// blob delivered to the LB microVM, not in the network-config.
