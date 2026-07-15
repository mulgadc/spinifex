package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/formation"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
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
		predastoreMultiNodeTemplate, nodes, "AK", "SK", "ap-southeast-2", "token", "/tmp", "0.0.0.0", 0,
	)
	require.NoError(t, err)
	assert.Contains(t, content, `nats_url = "nats://localhost:4222"`)
	assert.NotContains(t, content, `nats_url = "nats://0.0.0.0:4222"`)

	// Specific bind IP → stays as-is.
	content2, err := admin.GenerateMultiNodePredastoreConfig(
		predastoreMultiNodeTemplate, nodes, "AK", "SK", "ap-southeast-2", "token", "/tmp", "10.11.12.1", 0,
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

// A re-init must not rotate the per-node predastore key: rotating it would orphan
// every fragment already sealed under the old key. The helper preserves an
// existing key byte-for-byte.
func TestWritePredastoreEncryptionKey_PreservesExisting(t *testing.T) {
	configDir := t.TempDir()

	keyPath, err := writePredastoreEncryptionKey(configDir)
	require.NoError(t, err)
	orig, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	keyPath2, err := writePredastoreEncryptionKey(configDir)
	require.NoError(t, err)
	assert.Equal(t, keyPath, keyPath2)
	after, err := os.ReadFile(keyPath2)
	require.NoError(t, err)
	assert.Equal(t, orig, after, "existing predastore key must be preserved on re-init")
}

// ensureViperblockEncryptionKey generates a shared 32-byte 0600 key on a fresh
// dir and returns bytes that match the on-disk file; a second call preserves it
// so a re-init keeps every encrypted volume readable.
func TestEnsureViperblockEncryptionKey(t *testing.T) {
	configDir := t.TempDir()

	key, keyPath, err := ensureViperblockEncryptionKey(configDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(configDir, "viperblock", "encryption.key"), keyPath)
	assert.Len(t, key, 32, "viperblock key must be 32 bytes")

	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Equal(t, int64(32), info.Size(), "viperblock master key must be exactly 32 bytes")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "viperblock master key must be mode 0600")

	onDisk, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, key, onDisk, "returned bytes must match the file")

	key2, _, err := ensureViperblockEncryptionKey(configDir)
	require.NoError(t, err)
	assert.Equal(t, key, key2, "existing viperblock key must be preserved on re-init")
}

// A present-but-corrupt viperblock key must fail loud rather than regenerate,
// which would orphan every already-encrypted volume in the cluster.
func TestEnsureViperblockEncryptionKey_CorruptFailsLoud(t *testing.T) {
	configDir := t.TempDir()
	keyDir := filepath.Join(configDir, "viperblock")
	require.NoError(t, os.MkdirAll(keyDir, 0750))
	keyPath := filepath.Join(keyDir, "encryption.key")
	require.NoError(t, os.WriteFile(keyPath, []byte("bad"), 0600))

	_, _, err := ensureViperblockEncryptionKey(configDir)
	require.Error(t, err, "a truncated viperblock key must error, not regenerate")

	after, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("bad"), after, "corrupt viperblock key must not be overwritten")
}

// ensureMasterKey generates a fresh in-memory key without touching disk (the
// bootstrap writer persists it), and preserves an existing on-disk key so a
// re-init never rotates the key that encrypts IAM secrets in NATS KV.
func TestEnsureMasterKey(t *testing.T) {
	t.Run("FreshGeneratesWithoutWriting", func(t *testing.T) {
		configDir := t.TempDir()
		key, existed, err := ensureMasterKey(configDir)
		require.NoError(t, err)
		assert.False(t, existed, "no master.key present yet")
		assert.Len(t, key, 32)
		assert.NoFileExists(t, filepath.Join(configDir, "master.key"),
			"ensureMasterKey must not persist on the fresh path")
	})

	t.Run("PreservesExisting", func(t *testing.T) {
		configDir := t.TempDir()
		seed := make([]byte, 32)
		for i := range seed {
			seed[i] = byte(200 - i)
		}
		require.NoError(t, handlers_iam.SaveMasterKey(filepath.Join(configDir, "master.key"), seed))

		key, existed, err := ensureMasterKey(configDir)
		require.NoError(t, err)
		assert.True(t, existed, "existing master.key must be detected")
		assert.Equal(t, seed, key, "existing master key must be loaded verbatim")
	})

	// A present-but-corrupt master.key must fail loud, never be silently
	// regenerated: rotating it would orphan every IAM secret in NATS KV.
	t.Run("CorruptFailsLoud", func(t *testing.T) {
		configDir := t.TempDir()
		keyPath := filepath.Join(configDir, "master.key")
		require.NoError(t, os.WriteFile(keyPath, []byte("too-short"), 0600))

		_, _, err := ensureMasterKey(configDir)
		require.Error(t, err, "a truncated master.key must error, not regenerate")

		after, err := os.ReadFile(keyPath)
		require.NoError(t, err)
		assert.Equal(t, []byte("too-short"), after, "corrupt master key must not be overwritten")
	})
}

// The identity bundle is preserved holistically on re-init: the load helpers
// recover the exact system credentials and never rewrite bootstrap.json. Admin
// credentials are not recovered from disk — awsgw consumes and deletes
// bootstrap.json after first boot, so the preserve path must not read it back.
func TestPreservedIdentityBundle(t *testing.T) {
	configDir := t.TempDir()
	bootstrapDir := filepath.Join(configDir, "awsgw")

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	const (
		sysAccess   = "AKIASYSTEM0000000000"
		sysSecret   = "system-secret-value"
		adminAccess = "AKIAADMIN00000000000"
		adminSecret = "admin-secret-value"
		accountID   = "123456789012"
	)
	require.NoError(t, writeBootstrapFilesWithAdmin(configDir, bootstrapDir, masterKey,
		sysAccess, sysSecret, accountID, adminAccess, adminSecret))
	require.NoError(t, writeSystemCredentials(configDir, sysAccess, sysSecret))

	bootstrapPath := filepath.Join(bootstrapDir, "bootstrap.json")
	bootstrapBefore, err := os.ReadFile(bootstrapPath)
	require.NoError(t, err)

	// master.key round-trips and is flagged as pre-existing.
	loadedKey, existed, err := ensureMasterKey(configDir)
	require.NoError(t, err)
	assert.True(t, existed)
	assert.Equal(t, masterKey, loadedKey)

	// System credentials come back verbatim (they must match the NATS KV seed).
	gotSysAccess, gotSysSecret, err := loadSystemCredentials(configDir)
	require.NoError(t, err)
	assert.Equal(t, sysAccess, gotSysAccess)
	assert.Equal(t, sysSecret, gotSysSecret)

	// The preserve helpers must not rewrite the seed file.
	bootstrapAfter, err := os.ReadFile(bootstrapPath)
	require.NoError(t, err)
	assert.Equal(t, bootstrapBefore, bootstrapAfter, "bootstrap.json must not be rewritten on re-init")
}

// loadSystemCredentials surfaces a clear error when the file is absent rather
// than returning empty credentials that would silently break SigV4 auth.
func TestLoadSystemCredentials_Missing(t *testing.T) {
	_, _, err := loadSystemCredentials(t.TempDir())
	require.Error(t, err)
}

// A present-but-empty or malformed system-credentials.json must error rather
// than yield blank credentials that would silently break SigV4 between services.
func TestLoadSystemCredentials_Invalid(t *testing.T) {
	t.Run("EmptyValues", func(t *testing.T) {
		configDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "system-credentials.json"),
			[]byte(`{"access_key_id":"","secret_access_key":""}`), 0600))
		_, _, err := loadSystemCredentials(configDir)
		require.Error(t, err)
	})

	t.Run("MalformedJSON", func(t *testing.T) {
		configDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "system-credentials.json"),
			[]byte("not json"), 0600))
		_, _, err := loadSystemCredentials(configDir)
		require.Error(t, err)
	})
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
		Pools: []admin.PoolData{{
			Name: "wan", Source: "dhcp", BindBridge: "br-wan", PrefixLen: 24,
		}},
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
		Pools: []admin.PoolData{{
			Name: "wan", Source: "static",
			Start: "192.168.1.150", End: "192.168.1.250",
			Gateway: "192.168.1.1", PrefixLen: 24,
		}},
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
	require.Len(t, settings.Pools, 1)
	assert.Equal(t, "dhcp", settings.Pools[0].Source)
	assert.Equal(t, "br-wan", settings.Pools[0].BindBridge)
}

func renderSingleNodePredastore(t *testing.T, settings admin.ConfigSettings) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "predastore.toml")
	require.NoError(t, admin.GenerateConfigFile(path, predastoreTomlTemplate, settings))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func predastoreMultinodeNodes() []admin.PredastoreNodeConfig {
	return []admin.PredastoreNodeConfig{
		{ID: 1, Host: "10.0.0.1"}, {ID: 2, Host: "10.0.0.2"}, {ID: 3, Host: "10.0.0.3"},
	}
}

// Unset knob must leave production config byte-identical: no [compaction] block
// and the original trailing bytes unchanged (single-node has no trailing
// newline, multinode keeps one).
func TestPredastoreTemplates_UnsetCompactionEmitsNoBlock(t *testing.T) {
	single := renderSingleNodePredastore(t, admin.ConfigSettings{
		Region: "ap-southeast-2", BindIP: "0.0.0.0", ConfigDir: "/cfg",
		AccessKey: "AK", SecretKey: "SK", NatsToken: "tok",
	})
	assert.NotContains(t, single, "[compaction]")
	assert.True(t, strings.HasSuffix(single, `access_keys_bucket = "spinifex-iam-access-keys"`),
		"unset single-node tail changed: %q", single[len(single)-40:])

	multi, err := admin.GenerateMultiNodePredastoreConfig(
		predastoreMultiNodeTemplate, predastoreMultinodeNodes(), "AK", "SK", "ap-southeast-2", "tok", "/cfg", "10.0.0.1", 0,
	)
	require.NoError(t, err)
	assert.NotContains(t, multi, "[compaction]")
	assert.True(t, strings.HasSuffix(multi, "access_keys_bucket = \"spinifex-iam-access-keys\"\n"),
		"unset multinode tail changed: %q", multi[len(multi)-40:])
}

func TestPredastoreTemplates_SetCompactionEmitsParseableBlock(t *testing.T) {
	const interval = 30

	single := renderSingleNodePredastore(t, admin.ConfigSettings{
		Region: "ap-southeast-2", BindIP: "0.0.0.0", ConfigDir: "/cfg",
		AccessKey: "AK", SecretKey: "SK", NatsToken: "tok", CompactionIntervalSeconds: interval,
	})
	assert.Contains(t, single, "[compaction]")
	var singleCfg compactionConfig
	require.NoError(t, toml.Unmarshal([]byte(single), &singleCfg))
	assert.Equal(t, interval, singleCfg.Compaction.IntervalSeconds)

	multi, err := admin.GenerateMultiNodePredastoreConfig(
		predastoreMultiNodeTemplate, predastoreMultinodeNodes(), "AK", "SK", "ap-southeast-2", "tok", "/cfg", "10.0.0.1", interval,
	)
	require.NoError(t, err)
	var multiCfg compactionConfig
	require.NoError(t, toml.Unmarshal([]byte(multi), &multiCfg))
	assert.Equal(t, interval, multiCfg.Compaction.IntervalSeconds)
}

// compactionConfig parses just the rendered [compaction] block. Predastore's
// s3.Config dropped its Compaction field, so this asserts the spinifex template
// still emits a valid, parseable block carrying the interval.
type compactionConfig struct {
	Compaction struct {
		IntervalSeconds int `toml:"interval_seconds"`
	} `toml:"compaction"`
}

// The init/join flag must register with an unset (0) default and flow into the
// rendered config via ConfigSettings.
func TestAdminInitFlag_CompactionIntervalReachesRenderedConfig(t *testing.T) {
	for _, c := range []*cobra.Command{adminInitCmd, adminJoinCmd} {
		f := c.Flags().Lookup("predastore-compaction-interval")
		require.NotNil(t, f, "%s missing --predastore-compaction-interval", c.Name())
		assert.Equal(t, "0", f.DefValue)
	}

	require.NoError(t, adminInitCmd.Flags().Set("predastore-compaction-interval", "45"))
	defer func() { _ = adminInitCmd.Flags().Set("predastore-compaction-interval", "0") }()

	v, _ := adminInitCmd.Flags().GetInt("predastore-compaction-interval")
	require.Equal(t, 45, v)

	out := renderSingleNodePredastore(t, admin.ConfigSettings{
		Region: "ap-southeast-2", BindIP: "0.0.0.0", CompactionIntervalSeconds: v,
	})
	assert.Contains(t, out, "interval_seconds = 45")
}

// The image is written into a root volume of exactly the size this returns, so
// anything less than the image size truncates the image and the guest never
// finds its root. The old flooring conversion undersized every non-round image.
func TestAMIVolumeSizeGiB(t *testing.T) {
	const giB int64 = 1024 * 1024 * 1024

	tests := []struct {
		name  string
		bytes int64
		want  uint64
	}{
		// The regression: a real 4.94 GiB mkosi image floored to 4 GiB and the
		// guest hung with no root partition.
		{name: "non-round image rounds up", bytes: 5306470400, want: 5},
		// Exact multiples must not gain a spare GiB — the round sizes are the
		// common case and were the only ones the old code got right.
		{name: "exact multiple is unchanged", bytes: 16 * giB, want: 16},
		{name: "one byte over a multiple rounds up", bytes: 16*giB + 1, want: 17},
		{name: "one byte under a multiple rounds up", bytes: 16*giB - 1, want: 16},
		// Sub-GiB images still need a whole GiB to live in.
		{name: "sub-GiB image gets a full GiB", bytes: 1, want: 1},
		{name: "empty image", bytes: 0, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := amiVolumeSizeGiB(tt.bytes)
			assert.Equal(t, tt.want, got)
			// The invariant the guest depends on, stated directly.
			if tt.bytes > 0 {
				assert.GreaterOrEqual(t, int64(got)*giB, tt.bytes,
					"volume must be large enough to hold the image")
			}
		})
	}
}
