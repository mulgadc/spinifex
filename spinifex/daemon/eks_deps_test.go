package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveGatewayHost covers all five host-selection branches plus the
// no-reachable-host fallthrough. resolveGatewayHost is the single source of
// truth for the OIDC issuer host, EKS NATS URL, and lb-agent gateway URL, so
// each branch is pinned here to prevent silent divergence (M7).
func TestResolveGatewayHost(t *testing.T) {
	tests := []struct {
		name        string
		awsgwHost   string
		advertiseIP string
		mgmtBridge  string
		devNet      bool
		want        string
	}{
		{
			name:        "1_mgmt_dedicated_awsgw_ip",
			awsgwHost:   "10.20.0.5:9999",
			advertiseIP: "203.0.113.7",
			mgmtBridge:  "10.15.8.1",
			want:        "10.20.0.5",
		},
		{
			name:        "1_skipped_when_bind_equals_advertise_falls_to_advertise",
			awsgwHost:   "203.0.113.7:9999",
			advertiseIP: "203.0.113.7",
			mgmtBridge:  "10.15.8.1",
			want:        "203.0.113.7",
		},
		{
			name:       "1_skipped_when_bind_loopback_falls_to_mgmt",
			awsgwHost:  "127.0.0.1:9999",
			mgmtBridge: "10.15.8.1",
			want:       "10.15.8.1",
		},
		{
			name:        "2_advertise_ip",
			awsgwHost:   "0.0.0.0:9999",
			advertiseIP: "203.0.113.7",
			want:        "203.0.113.7",
		},
		{
			name:       "3_mgmt_bridge_when_awsgw_wildcard",
			awsgwHost:  "0.0.0.0:9999",
			mgmtBridge: "10.15.8.1",
			want:       "10.15.8.1",
		},
		{
			name:      "4_dev_networking_shim",
			awsgwHost: "0.0.0.0:9999",
			devNet:    true,
			want:      "10.0.2.2",
		},
		{
			name:      "5_awsgw_specific_ip_no_mgmt_no_advertise",
			awsgwHost: "198.51.100.9:9999",
			want:      "198.51.100.9",
		},
		{
			name:      "6_no_reachable_host",
			awsgwHost: "0.0.0.0:9999",
			want:      "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{
				mgmtBridgeIP: tc.mgmtBridge,
				config: &config.Config{
					AdvertiseIP: tc.advertiseIP,
					AWSGW:       config.AWSGWConfig{Host: tc.awsgwHost},
					Daemon:      config.DaemonConfig{DevNetworking: tc.devNet},
				},
			}
			assert.Equal(t, tc.want, d.resolveGatewayHost())
		})
	}
}

// TestResolveSystemPredastoreURL covers the mgmt-bridge-present,
// mgmt-bridge-absent, and no-config branches: a guest system VM (the K3s
// server) can only reach predastore via the mgmt bridge, so the local
// loopback-rewritten Predastore.Host must never leak into the URL handed to
// a guest.
func TestResolveSystemPredastoreURL(t *testing.T) {
	tests := []struct {
		name           string
		predastoreHost string
		mgmtBridge     string
		want           string
	}{
		{
			name:           "mgmt_bridge_present_uses_bridge_ip_and_configured_port",
			predastoreHost: "127.0.0.1:9443",
			mgmtBridge:     "10.15.8.1",
			want:           "https://10.15.8.1:9443",
		},
		{
			name:           "mgmt_bridge_present_defaults_port_when_host_has_none",
			predastoreHost: "predastore-host-no-port",
			mgmtBridge:     "10.15.8.1",
			want:           "https://10.15.8.1:8443",
		},
		{
			name:           "no_mgmt_bridge_falls_back_to_configured_host",
			predastoreHost: "predastore.internal:8443",
			want:           "https://predastore.internal:8443",
		},
		{
			name: "no_mgmt_bridge_no_host_returns_empty",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{
				mgmtBridgeIP: tc.mgmtBridge,
				config: &config.Config{
					Predastore: config.PredastoreConfig{Host: tc.predastoreHost},
				},
			}
			assert.Equal(t, tc.want, d.resolveSystemPredastoreURL())
		})
	}
}

// TestBuildEKSServiceDeps pins that the mgmt-bridge-derived predastore URL and
// a non-nil snapshot object store actually reach EKSServiceDeps — RestoreSnapshot
// silently can't resolve "latest snapshot" without SnapshotStore, and CP launch
// silently can't write etcd-snapshot.env without SystemPredastoreURL.
func TestBuildEKSServiceDeps(t *testing.T) {
	d := &Daemon{
		mgmtBridgeIP: "10.15.8.1",
		config: &config.Config{
			Predastore: config.PredastoreConfig{
				Host:      "127.0.0.1:8443",
				AccessKey: "AKIAPREDASTORE",
				SecretKey: "pred-s3cr3t",
				Region:    "us-east-1",
			},
		},
	}

	deps := d.buildEKSServiceDeps()

	assert.Equal(t, "https://10.15.8.1:8443", deps.SystemPredastoreURL)
	assert.NotNil(t, deps.SnapshotStore)
}

// writeMasterKey drops a valid shared master key beside configPath.
func writeMasterKey(t *testing.T, configPath string) {
	t.Helper()
	key := bytes.Repeat([]byte{0x42}, masterkey.MasterKeySize)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(configPath), "master.key"), key, 0o640))
}

func TestSystemRoleEnsurer(t *testing.T) {
	t.Run("no master key falls back to static creds", func(t *testing.T) {
		d := &Daemon{ctx: t.Context(), configPath: filepath.Join(t.TempDir(), "spinifex.toml")}
		assert.Nil(t, d.systemRoleEnsurer())
	})

	t.Run("IAM init failure is retried rather than cached", func(t *testing.T) {
		nc, err := nats.Connect(sharedNATSURL)
		require.NoError(t, err)
		defer nc.Close()

		d := &Daemon{ctx: t.Context(), natsConn: nc, configPath: filepath.Join(t.TempDir(), "spinifex.toml")}
		writeMasterKey(t, d.configPath)
		assert.Nil(t, d.systemRoleEnsurer(), "a NATS server without JetStream cannot back IAM")
		assert.Nil(t, d.iamEnsurerCached)
	})

	t.Run("success is built once and cached", func(t *testing.T) {
		clustersize.DeclareForTest(t, 1)
		nc, err := nats.Connect(sharedJSNATSURL)
		require.NoError(t, err)
		defer nc.Close()

		d := &Daemon{ctx: t.Context(), natsConn: nc, configPath: filepath.Join(t.TempDir(), "spinifex.toml")}
		writeMasterKey(t, d.configPath)
		first := d.systemRoleEnsurer()
		require.NotNil(t, first)

		// The key going away after a successful build must not matter.
		require.NoError(t, os.Remove(filepath.Join(filepath.Dir(d.configPath), "master.key")))
		assert.Same(t, first, d.systemRoleEnsurer())
	})
}

func TestBuildEKSServiceDeps_ConfigDerivedFields(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----"), 0o600))

	base := func() *Daemon {
		return &Daemon{
			mgmtBridgeIP:  "10.15.8.1",
			clusterConfig: &config.ClusterConfig{AWS: config.AWSConfig{ServicesDomain: "svc.example"}},
			volumeService: &ec2volume.VolumeServiceImpl{},
			config: &config.Config{
				AdvertiseIP: "192.0.2.10",
				AWSGW:       config.AWSGWConfig{Host: "0.0.0.0:8443"},
				NATS:        config.NATSConfig{CACert: caPath},
			},
		}
	}

	t.Run("readable CA and gateway port", func(t *testing.T) {
		deps := base().buildEKSServiceDeps()
		assert.Equal(t, "svc.example", deps.InternalSuffix)
		assert.Equal(t, "-----BEGIN CERTIFICATE-----", deps.GatewayCACert)
		assert.Equal(t, "https://192.0.2.10:8443", deps.GatewayBaseURL)
		assert.Equal(t, "https://10.15.8.1:8443", deps.SystemGatewayURL)
		assert.NotNil(t, deps.Volume, "a configured volume service must reach CSI reclaim")
	})

	t.Run("unreadable CA leaves the gateway unverified", func(t *testing.T) {
		d := base()
		d.config.NATS.CACert = filepath.Join(t.TempDir(), "missing.pem")
		deps := d.buildEKSServiceDeps()
		assert.Empty(t, deps.GatewayCACert)
	})
}
