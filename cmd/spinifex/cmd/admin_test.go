package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/formation"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression guard: source URL must print for every error kind, not just
// mismatch, so a 404/non-HTTPS/size-cap failure still tells the operator
// which URL to investigate.
func TestPrintChecksumError(t *testing.T) {
	image := utils.Images{Checksum: "https://example.com/SUMS", ChecksumType: "sha512"}
	const imageFile = "/var/lib/img.tar.xz"
	const imageName = "debian-12-x86_64"

	errs := []error{
		fmt.Errorf("%w: expected abc got def", utils.ErrChecksumMismatch),
		fmt.Errorf("%w: 404", utils.ErrChecksumFetchFailed),
		errors.New("open /x: no such file"),
	}
	for _, e := range errs {
		var buf bytes.Buffer
		printChecksumError(&buf, imageFile, imageName, image, e)
		out := buf.String()
		assert.Contains(t, out, image.Checksum, "source URL must print for: %v", e)
		assert.Contains(t, out, imageFile)
		assert.Contains(t, out, "spx admin images import --name "+imageName+" --force")
	}
}

// buildRemoteNodes must prefer AdvertiseIP (off-host dial target) and fall
// back to BindIP when the peer pre-dates siv-8 and didn't send AdvertiseIP.
func TestBuildRemoteNodes_AdvertiseFallback(t *testing.T) {
	nodes := map[string]formation.NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1", AdvertiseIP: "203.0.113.1"},
		"node2": {Name: "node2", BindIP: "10.0.0.2"}, // legacy joiner
		"node3": {Name: "node3", BindIP: "10.0.0.3", AdvertiseIP: "203.0.113.3"},
	}
	got := buildRemoteNodes(nodes, "node3")
	if assert.Len(t, got, 2) {
		assert.Equal(t, "node1", got[0].Name)
		assert.Equal(t, "203.0.113.1", got[0].Host, "advertise wins when set")
		assert.Equal(t, "node2", got[1].Name)
		assert.Equal(t, "10.0.0.2", got[1].Host, "bind fallback when advertise empty")
	}
}

func TestResolveAdvertiseIP(t *testing.T) {
	wan := &admin.DetectedNetwork{WAN: &admin.DetectedInterface{IP: "192.168.1.21"}}
	noWAN := &admin.DetectedNetwork{}

	tests := []struct {
		name      string
		bindIP    string
		advertise string
		detected  *admin.DetectedNetwork
		want      string
		wantErr   bool
	}{
		{"single-node default, WAN detected", "0.0.0.0", "", wan, "192.168.1.21", false},
		{"single-node default, no WAN", "0.0.0.0", "", noWAN, "127.0.0.1", false},
		{"single-node explicit advertise", "0.0.0.0", "203.0.113.5", wan, "203.0.113.5", false},
		{"multi-node init, specific bind", "10.11.12.1", "", nil, "10.11.12.1", false},
		{"multi-node dual-homed override", "10.11.12.1", "203.0.113.5", nil, "203.0.113.5", false},
		{"loopback-only test", "127.0.0.1", "", noWAN, "127.0.0.1", false},
		{"invalid advertise flag", "0.0.0.0", "not-an-ip", wan, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAdvertiseIP(tt.bindIP, tt.advertise, tt.detected)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Generated spinifex.toml must include both host (listen, BindIP) and
// advertise (off-host dial target, AdvertiseIP) for the local node.
func TestSpinifexTomlTemplate_AdvertiseField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	settings := admin.ConfigSettings{
		Node:        "node1",
		Az:          "ap-southeast-2a",
		Port:        "4432",
		Region:      "ap-southeast-2",
		BindIP:      "0.0.0.0",
		AdvertiseIP: "192.168.1.21",
		AccessKey:   "AKIATEST",
		SecretKey:   "SECRET",
		AccountID:   "123456789012",
		NatsToken:   "token",
		ConfigDir:   dir,
		OVNNBAddr:   "tcp:127.0.0.1:6641",
		OVNSBAddr:   "tcp:127.0.0.1:6642",
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `host = "0.0.0.0"`, "listen address")
	assert.Contains(t, content, `advertise = "192.168.1.21"`, "off-host dial target")
}

// Empty AdvertiseIP (e.g. loading an existing cluster pre-siv-8) must NOT
// render an empty advertise = "" line — downstream fallback to Host kicks in.
func TestSpinifexTomlTemplate_AdvertiseOmittedWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	settings := admin.ConfigSettings{
		Node:      "node1",
		Az:        "ap-southeast-2a",
		Port:      "4432",
		Region:    "ap-southeast-2",
		BindIP:    "10.11.12.1",
		AccessKey: "AKIATEST",
		SecretKey: "SECRET",
		AccountID: "123456789012",
		NatsToken: "token",
		ConfigDir: dir,
		OVNNBAddr: "tcp:127.0.0.1:6641",
		OVNSBAddr: "tcp:127.0.0.1:6642",
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))
	data, _ := os.ReadFile(path)
	assert.NotContains(t, string(data), "advertise =")
}

// detectedDhcpBindBridge must return the default-route interface name verbatim
// when it's a bridge (Linux or OVS, br-* prefix). The old detectedWanBridge()
// returned hardcoded "br-ext" for Linux bridges — that value broke DHCP on
// consumer-router LANs because br-ext never sees LAN DHCP traffic (mulga-998).
func TestDetectedDhcpBindBridge_LinuxBridgeDefaultRoute(t *testing.T) {
	detected := &admin.DetectedNetwork{WAN: &admin.DetectedInterface{Name: "br-wan"}}
	assert.Equal(t, "br-wan", detectedDhcpBindBridge(detected),
		"Linux bridge (WAN NIC enslaved) must return the bridge name, not 'br-ext'")
}

func TestDetectedDhcpBindBridge_OVSBridgeDefaultRoute(t *testing.T) {
	detected := &admin.DetectedNetwork{WAN: &admin.DetectedInterface{Name: "br-ext"}}
	assert.Equal(t, "br-ext", detectedDhcpBindBridge(detected),
		"OVS bridge (WAN NIC on br-*) returned verbatim")
}

func TestDetectedDhcpBindBridge_PhysicalNIC(t *testing.T) {
	detected := &admin.DetectedNetwork{WAN: &admin.DetectedInterface{Name: "enp0s3"}}
	assert.Equal(t, "br-wan", detectedDhcpBindBridge(detected),
		"bare NIC defaults to 'br-wan' (the bridge the installer will create)")
}

func TestDetectedDhcpBindBridge_NilWAN(t *testing.T) {
	assert.Empty(t, detectedDhcpBindBridge(nil))
	assert.Empty(t, detectedDhcpBindBridge(&admin.DetectedNetwork{}))
}

// Legacy `wan_bridge` TOML key must fail-start vpcd with guidance, not silently
// alias (per mulga-998 D3). Prevents the footgun where operators inherited the
// old key pointing at 'br-ext' and got broken DHCP on veth-mode hosts.
func TestCheckLegacyWanBridgeKey_TOMLRejected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
node = "node1"
[nodes.node1.vpcd]
ovn_nb_addr = "tcp:127.0.0.1:6641"
wan_bridge = "br-ext"
`), 0o644))

	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetConfigFile(cfgPath)
	viper.SetConfigType("toml")
	require.NoError(t, viper.ReadInConfig())

	err := checkLegacyWanBridgeKey("node1", cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dhcp_bind_bridge")
	assert.Contains(t, err.Error(), "wan_bridge")
}

func TestCheckLegacyWanBridgeKey_EnvVarRejected(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("SPINIFEX_VPCD_WAN_BRIDGE", "br-ext")

	err := checkLegacyWanBridgeKey("node1", "/tmp/unused.toml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SPINIFEX_VPCD_WAN_BRIDGE")
	assert.Contains(t, err.Error(), "dhcp_bind_bridge")
}

func TestCheckLegacyWanBridgeKey_CleanConfigPasses(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
node = "node1"
[nodes.node1.vpcd]
ovn_nb_addr = "tcp:127.0.0.1:6641"
dhcp_bind_bridge = "br-wan"
`), 0o644))

	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetConfigFile(cfgPath)
	viper.SetConfigType("toml")
	require.NoError(t, viper.ReadInConfig())

	assert.NoError(t, checkLegacyWanBridgeKey("node1", cfgPath))
}

// iam.nats_url must not embed a bare 0.0.0.0 — the predastore process dials
// its own NATS via loopback when listening on wildcard.
func TestPredastoreMultinodeTemplate_NatsURLLoopbackShim(t *testing.T) {
	nodes := []admin.PredastoreNodeConfig{
		{ID: 1, Host: "10.0.0.1"},
		{ID: 2, Host: "10.0.0.2"},
		{ID: 3, Host: "10.0.0.3"},
	}
	content, err := admin.GenerateMultiNodePredastoreConfig(
		predastoreMultiNodeTemplate, nodes, "AK", "SK", "ap-southeast-2", "token", "/tmp", "0.0.0.0",
	)
	require.NoError(t, err)
	assert.Contains(t, content, `nats_url = "nats://localhost:4222"`)
	assert.NotContains(t, content, `nats_url = "nats://0.0.0.0:4222"`)

	// Specific bind IP → stays as-is.
	content2, err := admin.GenerateMultiNodePredastoreConfig(
		predastoreMultiNodeTemplate, nodes, "AK", "SK", "ap-southeast-2", "token", "/tmp", "10.11.12.1",
	)
	require.NoError(t, err)
	assert.Contains(t, content2, `nats_url = "nats://10.11.12.1:4222"`)
}
