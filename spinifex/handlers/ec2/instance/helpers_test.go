package handlers_ec2_instance

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- generateNetworkConfig ---

func TestGenerateNetworkConfig_BothEmpty(t *testing.T) {
	cfg := generateNetworkConfig("", "", "", "", nil, true)
	assert.Equal(t, cloudInitNetworkConfigWildcard, cfg)
}

func TestGenerateNetworkConfig_OneEmpty(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "", "", "", nil, true)
	assert.Contains(t, cfg, "vpc0:", "eniMAC alone should produce per-interface config")
	assert.NotContains(t, cfg, "dev0:", "no dev NIC without devMAC")
	// The IMDS on-link route rides vpc0 even with no dev NIC / extra ENIs — the
	// common production shape — so guard it here too, not only on the multi-NIC path.
	assert.Contains(t, cfg, "to: 169.254.169.254/32", "vpc0 must carry the IMDS on-link route")

	cfg = generateNetworkConfig("", "02:00:00:dd:ee:ff", "", "", nil, true)
	assert.Equal(t, cloudInitNetworkConfigWildcard, cfg, "should fall back to wildcard if eniMAC empty")
}

func TestGenerateNetworkConfig_DualNIC(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "", nil, true)
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
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:de:00:dd:ee:ff", "02:a0:00:11:22:33", "10.15.8.101", nil, true)
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
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "", "02:a0:00:11:22:33", "10.15.8.101", nil, true)
	assert.Contains(t, cfg, "vpc0:")
	assert.NotContains(t, cfg, "dev0:", "no dev NIC without devMAC")
	assert.Contains(t, cfg, "mgmt0:")
	assert.Contains(t, cfg, `macaddress: "02:a0:00:11:22:33"`)
	assert.Contains(t, cfg, `"10.15.8.101/24"`)
}

func TestGenerateNetworkConfig_MgmtMACWithoutIP(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:de:00:dd:ee:ff", "02:a0:00:11:22:33", "", nil, true)
	assert.NotContains(t, cfg, "mgmt0:", "mgmt NIC should not appear without IP")
}

func TestGenerateNetworkConfig_MgmtIPWithoutMAC(t *testing.T) {
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:de:00:dd:ee:ff", "", "10.15.8.101", nil, true)
	assert.NotContains(t, cfg, "mgmt0:", "mgmt NIC should not appear without MAC")
}

func TestGenerateNetworkConfig_IMDSOnLinkRoute(t *testing.T) {
	// The primary VPC NIC carries an on-link route to the IMDS metadata IP so the
	// guest ARPs 169.254.169.254 directly; the per-subnet localport answers.
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "",
		[]string{"02:00:00:bb:bb:bb"}, true)
	assert.Contains(t, cfg, "to: 169.254.169.254/32")
	assert.Contains(t, cfg, "scope: link")
	// Only the primary NIC (vpc0) gets it — not extra VPC NICs or the dev NIC,
	// matching AWS (IMDS reached via the primary ENI).
	assert.Equal(t, 1, strings.Count(cfg, "169.254.169.254/32"))
}

func TestGenerateNetworkConfig_IMDSRouteOmittedWhenDisabled(t *testing.T) {
	// Alpine's eni renderer crashes on a gateway-less route, so the route is
	// omitted from network-config and delivered out-of-band (see
	// buildAlpineCloudInit). emitIMDSRoute=false must drop it entirely.
	cfg := generateNetworkConfig("02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "", nil, false)
	assert.Contains(t, cfg, "vpc0:")
	assert.NotContains(t, cfg, "169.254.169.254/32", "route must not render when emitIMDSRoute=false")
	assert.NotContains(t, cfg, "      routes:", "no vpc0 routes block when the only route is the IMDS one")
}

// selectNetworkConfigForFamily must omit the IMDS route for Alpine (eni renderer
// can't handle the gateway-less route) but keep it for everyone else.
func TestSelectNetworkConfigForFamily_AlpineOmitsIMDSRoute(t *testing.T) {
	alpine := selectNetworkConfigForFamily("alpine", "02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "", nil)
	assert.NotContains(t, alpine, "169.254.169.254/32", "Alpine network-config must not carry the IMDS route")

	deb := selectNetworkConfigForFamily("debian", "02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff", "", "", nil)
	assert.Contains(t, deb, "169.254.169.254/32", "non-Alpine families keep the netplan IMDS route")
}

// buildAlpineCloudInit delivers the IMDS on-link route via a persistent
// /etc/local.d script + OpenRC local service, since the eni renderer can't.
func TestBuildAlpineCloudInit(t *testing.T) {
	wf, rc := buildAlpineCloudInit("02:00:00:aa:bb:cc")
	assert.Contains(t, wf, "/etc/local.d/imds-onlink-route.start")
	assert.Contains(t, wf, "ip route show default")
	assert.Contains(t, wf, `ip route replace 169.254.169.254/32 dev "$dev" scope link`)
	assert.Contains(t, rc, "rc-update")
	assert.Contains(t, rc, "local")

	// No VPC NIC → nothing to add.
	wf, rc = buildAlpineCloudInit("")
	assert.Empty(t, wf)
	assert.Empty(t, rc)
}

// Route for multi-node LB mgmt traffic is handled via the fw_cfg netcfg
// blob delivered to the LB microVM, not in the network-config.
