package cmd

import (
	"os"
	"path/filepath"
	"strings"
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

		ExternalMode: "nat",
		BridgeMode:   "nat",
		Pools: []admin.PoolData{{
			Name: "nat-transit", Gateway: "100.127.0.1", PrefixLen: 24,
		}},
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
		Pools: []admin.PoolData{{
			Name: "wan", Source: "static",
			Start: "192.168.1.150", End: "192.168.1.250",
			Gateway: "192.168.1.1", PrefixLen: 24,
		}},
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

func TestBridgeModeFor(t *testing.T) {
	assert.Equal(t, "nat", bridgeModeFor("nat"))
	assert.Equal(t, "", bridgeModeFor("pool"))
	assert.Equal(t, "", bridgeModeFor(""))
}

func TestSpinifexTomlTemplate_NATModeWithPublicPool(t *testing.T) {
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

		ExternalMode: "nat",
		BridgeMode:   "nat",
		Pools: []admin.PoolData{
			{Name: "nat-transit", Gateway: "100.127.0.1", PrefixLen: 24},
			{
				Name: "wan", Source: "static",
				Start: "192.168.1.150", End: "192.168.1.250",
				Gateway: "192.168.1.1", PrefixLen: 24,
			},
		},
	}
	require.NoError(t, admin.GenerateConfigFile(path, spinifexTomlTemplate, settings))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `name        = "nat-transit"`)
	assert.Contains(t, content, `name        = "wan"`)
	assert.Contains(t, content, `range_start = "192.168.1.150"`)
	assert.Contains(t, content, `range_end   = "192.168.1.250"`)
	transitIdx := strings.Index(content, `name        = "nat-transit"`)
	wanIdx := strings.Index(content, `name        = "wan"`)
	assert.Less(t, transitIdx, wanIdx, "transit pool must render first (IGW allocator picks it)")
}

func TestResolvePublicPoolFlags(t *testing.T) {
	tests := []struct {
		name              string
		source, poolRange string
		bindBridge, gw    string
		defaultBridge     string
		wantSource        string
		wantStart         string
		wantEnd           string
		wantBridge        string
		wantErr           string
	}{
		{
			name: "static pool range", source: "", poolRange: "192.168.1.150-192.168.1.250", gw: "192.168.1.1",
			wantSource: "static", wantStart: "192.168.1.150", wantEnd: "192.168.1.250",
		},
		{
			name: "dhcp defaults bind bridge", source: "dhcp", defaultBridge: "wlan0",
			wantSource: "dhcp", wantBridge: "wlan0",
		},
		{
			name: "dhcp explicit bind bridge", source: "dhcp", bindBridge: "br-wan", defaultBridge: "wlan0",
			wantSource: "dhcp", wantBridge: "br-wan",
		},
		{
			name: "dhcp rejects pool range", source: "dhcp", poolRange: "192.168.1.150-192.168.1.250",
			wantErr: "--external-pool not allowed",
		},
		{
			name: "static requires gateway", poolRange: "192.168.1.150-192.168.1.250",
			wantErr: "--external-gateway is required",
		},
		{
			name: "static rejects bind bridge", poolRange: "192.168.1.150-192.168.1.250", gw: "192.168.1.1", bindBridge: "br-wan",
			wantErr: "--external-bind-bridge only valid",
		},
		{
			name: "static requires range", source: "static", gw: "192.168.1.1",
			wantErr: "--external-pool is required",
		},
		{
			name: "bad range", poolRange: "not-a-range", gw: "192.168.1.1",
			wantErr: "start-end IPs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, start, end, bb, err := resolvePublicPoolFlags(tt.source, tt.poolRange, tt.bindBridge, tt.gw, tt.defaultBridge)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSource, src)
			assert.Equal(t, tt.wantStart, start)
			assert.Equal(t, tt.wantEnd, end)
			assert.Equal(t, tt.wantBridge, bb)
		})
	}
}
