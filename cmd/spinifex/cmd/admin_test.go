package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
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
	const imageName = "debian-13-x86_64"

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

// The northstar stanza carries the non-secret domains so confined services
// (vpcd) resolve DNS names without reading the 0600 northstar.toml.
func TestSpinifexTomlTemplate_NorthstarDomains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	settings := admin.ConfigSettings{
		Node:                    "node1",
		Az:                      "ap-southeast-2a",
		Port:                    "4432",
		Region:                  "ap-southeast-2",
		BindIP:                  "10.11.12.1",
		AccessKey:               "AKIATEST",
		SecretKey:               "SECRET",
		AccountID:               "123456789012",
		NatsToken:               "token",
		ConfigDir:               dir,
		OVNNBAddr:               "tcp:127.0.0.1:6641",
		OVNSBAddr:               "tcp:127.0.0.1:6642",
		NorthstarConfigPath:     "/etc/spinifex/northstar/northstar.toml",
		NorthstarDefaultDomain:  "spx3.net",
		NorthstarInternalDomain: "compute.internal",
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `config_path = "/etc/spinifex/northstar/northstar.toml"`)
	assert.Contains(t, content, `default_domain = "spx3.net"`)
	assert.Contains(t, content, `internal_domain = "compute.internal"`)

	// The stanza must round-trip into NorthstarConfig so the resolvers read it.
	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "spx3.net", cfg.Nodes["node1"].Northstar.DefaultDomain)
	assert.Equal(t, "compute.internal", cfg.Nodes["node1"].Northstar.InternalDomain)
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
	assert.Contains(t, err.Error(), "wan_bridge")
	assert.Contains(t, err.Error(), "Remove")
}

func TestCheckLegacyWanBridgeKey_EnvVarRejected(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("SPINIFEX_VPCD_WAN_BRIDGE", "br-ext")

	err := checkLegacyWanBridgeKey("node1", "/tmp/unused.toml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SPINIFEX_VPCD_WAN_BRIDGE")
	assert.Contains(t, err.Error(), "Remove")
}

func TestCheckLegacyWanBridgeKey_CleanConfigPasses(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
node = "node1"
[nodes.node1.vpcd]
ovn_nb_addr = "tcp:127.0.0.1:6641"
external_interface = "enp0s3"
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

// writePredastoreEncryptionKey is called from both `spx admin init` and `spx
// admin join`; both depend on it producing a 32-byte file at 0600 in the
// expected layout. Predastore's keyfile loader rejects anything that isn't
// exactly that, so a regression here would break service startup on every
// node.
func TestWritePredastoreEncryptionKey(t *testing.T) {
	configDir := t.TempDir()

	keyPath, err := writePredastoreEncryptionKey(configDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(configDir, "predastore", "encryption.key"), keyPath)

	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Equal(t, int64(32), info.Size(), "predastore master key must be exactly 32 bytes")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "predastore master key must be mode 0600")

	// Per-node invariant: two successive calls (on what would be two
	// different nodes) must produce different key material. If they ever
	// returned the same bytes we'd have silently lost the per-node
	// blast-radius property.
	configDir2 := t.TempDir()
	keyPath2, err := writePredastoreEncryptionKey(configDir2)
	require.NoError(t, err)

	key1, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	key2, err := os.ReadFile(keyPath2)
	require.NoError(t, err)
	assert.NotEqual(t, key1, key2, "per-node keys must differ")
}

// The viperblock key gates at-rest encryption for every volume on a fresh
// install. masterkey.LoadShared rejects anything that isn't exactly 32 bytes at
// 0640-or-stricter, so a regression in size/mode would silently fall back to
// cleartext (empty path) or fail service startup.
func TestWriteViperblockEncryptionKey(t *testing.T) {
	configDir := t.TempDir()

	keyPath, err := writeViperblockEncryptionKey(configDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(configDir, "viperblock", "encryption.key"), keyPath)

	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Equal(t, int64(32), info.Size(), "viperblock master key must be exactly 32 bytes")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "viperblock master key must be mode 0600")
}

// Joiners persist the leader's shared key via saveViperblockEncryptionKey
// against a configDir with no viperblock/ subdir yet. The bytes round-trip
// verbatim — the whole cluster shares one key, so any mutation would orphan
// volumes sealed on other nodes.
func TestSaveViperblockEncryptionKey_RoundTrip(t *testing.T) {
	configDir := t.TempDir()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	keyPath, err := saveViperblockEncryptionKey(configDir, key)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(configDir, "viperblock", "encryption.key"), keyPath)

	got, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, key, got, "shared key must be written verbatim")
}

// A fresh install must render encryption_key_file so viperblockd enables
// at-rest encryption; an empty field (upgrade / legacy) must omit the line
// entirely so the volume stays in cleartext legacy mode.
func TestSpinifexTomlTemplate_EncryptionKeyFile(t *testing.T) {
	base := admin.ConfigSettings{
		Node: "node1", Az: "ap-southeast-2a", Port: "4432", Region: "ap-southeast-2",
		BindIP: "0.0.0.0", AccessKey: "AKIATEST", SecretKey: "SECRET",
		AccountID: "123456789012", NatsToken: "token",
		OVNNBAddr: "tcp:127.0.0.1:6641", OVNSBAddr: "tcp:127.0.0.1:6642",
	}

	dir := t.TempDir()
	withKey := base
	withKey.ConfigDir = dir
	withKey.EncryptionKeyFile = "/etc/spinifex/viperblock/encryption.key"
	path := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, withKey))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `encryption_key_file = "/etc/spinifex/viperblock/encryption.key"`)

	dir2 := t.TempDir()
	noKey := base
	noKey.ConfigDir = dir2
	path2 := filepath.Join(dir2, "spinifex.toml")
	require.NoError(t, admin.GenerateConfigFile(path2, spinifexTomlTemplate, noKey))
	data2, err := os.ReadFile(path2)
	require.NoError(t, err)
	assert.NotContains(t, string(data2), "encryption_key_file", "blank key must omit the field")
}

// Joiners run `spx admin join` against a configDir that doesn't yet contain
// a predastore/ subdir. The helper must create it (predastore subdir is
// owned by spinifex-storage in prod, but the helper just needs the dir to
// exist).
func TestWritePredastoreEncryptionKey_CreatesMissingDir(t *testing.T) {
	configDir := t.TempDir()
	// Don't pre-create the predastore subdir — simulate a fresh joiner.

	keyPath, err := writePredastoreEncryptionKey(configDir)
	require.NoError(t, err)

	dirInfo, err := os.Stat(filepath.Dir(keyPath))
	require.NoError(t, err)
	assert.True(t, dirInfo.IsDir())
}

// TestImagesRemoveCmd_FlagSchema guards the public flag surface for
// `spx admin images remove`. --image-id is required so cobra rejects a bare
// invocation; --force/--yes default to false.
func TestImagesRemoveCmd_FlagSchema(t *testing.T) {
	imageIDFlag := imagesRemoveCmd.Flags().Lookup("image-id")
	require.NotNil(t, imageIDFlag, "--image-id must be defined")
	assert.Equal(t, []string{"true"}, imageIDFlag.Annotations[cobraRequiredAnnotation],
		"--image-id must be marked required")

	forceFlag := imagesRemoveCmd.Flags().Lookup("force")
	require.NotNil(t, forceFlag)
	assert.Equal(t, "false", forceFlag.DefValue)

	yesFlag := imagesRemoveCmd.Flags().Lookup("yes")
	require.NotNil(t, yesFlag)
	assert.Equal(t, "false", yesFlag.DefValue)
}

// cobraRequiredAnnotation is the annotation key cobra uses to mark required flags.
const cobraRequiredAnnotation = "cobra_annotation_bash_completion_one_required_flag"

// `spx admin init` must expose --external-source and --external-bind-bridge so
// operators can pick the DHCP path for [[network.external_pools]].
func TestAdminInitCmd_ExternalDHCPFlagSchema(t *testing.T) {
	sourceFlag := adminInitCmd.Flags().Lookup("external-source")
	require.NotNil(t, sourceFlag, "--external-source must be defined")
	assert.Equal(t, "", sourceFlag.DefValue)

	bindFlag := adminInitCmd.Flags().Lookup("external-bind-bridge")
	require.NotNil(t, bindFlag, "--external-bind-bridge must be defined")
	assert.Equal(t, "", bindFlag.DefValue)

	poolFlag := adminInitCmd.Flags().Lookup("external-pool")
	require.NotNil(t, poolFlag)
	assert.Equal(t, "", poolFlag.DefValue)
}

// DHCP-sourced pool must emit source + bind_bridge into spinifex.toml so the
// daemon's validator and vpcd's DHCPManager find the bridge they DORA on.
func TestSpinifexTomlTemplate_ExternalPoolDHCPSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	settings := admin.ConfigSettings{
		Node:           "node1",
		Az:             "ap-southeast-2a",
		Port:           "4432",
		Region:         "ap-southeast-2",
		BindIP:         "10.11.12.1",
		ConfigDir:      dir,
		OVNNBAddr:      "tcp:127.0.0.1:6641",
		OVNSBAddr:      "tcp:127.0.0.1:6642",
		ExternalMode:   "pool",
		ExternalIface:  "eth0",
		PoolName:       "wan",
		PoolSource:     "dhcp",
		PoolBindBridge: "br-wan",
		PoolPrefixLen:  24,
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `source      = "dhcp"`)
	assert.Contains(t, content, `bind_bridge = "br-wan"`)
	assert.NotContains(t, content, "range_start", "dhcp pool must not emit range_start")
	assert.NotContains(t, content, "range_end", "dhcp pool must not emit range_end")
	assert.NotContains(t, content, "gateway     =", "gateway omitted for dhcp pool (discovered from OFFER)")
}

// Static-sourced pool must not emit bind_bridge — the validator rejects it.
func TestSpinifexTomlTemplate_ExternalPoolStaticSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	settings := admin.ConfigSettings{
		Node:          "node1",
		Az:            "ap-southeast-2a",
		Port:          "4432",
		Region:        "ap-southeast-2",
		BindIP:        "10.11.12.1",
		ConfigDir:     dir,
		OVNNBAddr:     "tcp:127.0.0.1:6641",
		OVNSBAddr:     "tcp:127.0.0.1:6642",
		ExternalMode:  "pool",
		ExternalIface: "eth0",
		PoolName:      "wan",
		PoolSource:    "static",
		PoolStart:     "192.168.1.150",
		PoolEnd:       "192.168.1.250",
		PoolGateway:   "192.168.1.1",
		PoolPrefixLen: 24,
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `source      = "static"`)
	assert.NotContains(t, content, "bind_bridge", "static pool must not emit bind_bridge")
	assert.Contains(t, content, `range_start = "192.168.1.150"`)
	assert.Contains(t, content, `gateway     = "192.168.1.1"`)
}

// Formation joiners must receive PoolBindBridge from the init node so the
// cluster-wide config reaches every chassis intact.
func TestApplyNetworkConfig_PropagatesPoolBindBridge(t *testing.T) {
	settings := &admin.ConfigSettings{}
	nc := &formation.NetworkConfig{
		ExternalMode:   "pool",
		PoolName:       "wan",
		PoolSource:     "dhcp",
		PoolBindBridge: "br-wan",
		PoolPrefixLen:  24,
	}
	applyNetworkConfig(settings, nc)
	assert.Equal(t, "dhcp", settings.PoolSource)
	assert.Equal(t, "br-wan", settings.PoolBindBridge)
}
