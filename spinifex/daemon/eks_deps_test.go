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
