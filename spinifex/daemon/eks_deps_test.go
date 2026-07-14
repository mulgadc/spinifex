package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
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
