package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpinifexTomlTemplate_NATMode(t *testing.T) {
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

		ExternalMode:  "nat",
		BridgeMode:    "nat",
		PoolName:      "nat-transit",
		PoolGateway:   "100.127.0.1",
		PoolPrefixLen: 24,
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `external_mode = "nat"`)
	assert.Contains(t, content, `bridge_mode = "nat"`)
	assert.Contains(t, content, `name        = "nat-transit"`)
	assert.Contains(t, content, `gateway     = "100.127.0.1"`)
	assert.NotContains(t, content, "range_start", "nat mode has no public IP range")
}

func TestSpinifexTomlTemplate_PoolModeOmitsBridgeMode(t *testing.T) {
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

		ExternalMode:  "pool",
		ExternalIface: "enp0s3",
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
	assert.NotContains(t, string(data), "bridge_mode", "bridged modes keep runtime auto-detect")
}

func TestIsNonBridgeableUplink(t *testing.T) {
	for _, name := range []string{"wlan0", "wlp3s0", "wlx001122334455", "wwan0", "wwp0s20f0u6", "ppp0"} {
		assert.True(t, isNonBridgeableUplink(name), name)
	}
	for _, name := range []string{"eth0", "enp0s3", "eno1", "br-wan", "veth-wan-br"} {
		assert.False(t, isNonBridgeableUplink(name), name)
	}
}

func TestBridgeModeAndPoolNameFor(t *testing.T) {
	assert.Equal(t, "nat", bridgeModeFor("nat"))
	assert.Equal(t, "", bridgeModeFor("pool"))
	assert.Equal(t, "", bridgeModeFor(""))
	assert.Equal(t, "nat-transit", poolNameFor("nat"))
	assert.Equal(t, "wan", poolNameFor("pool"))
}
