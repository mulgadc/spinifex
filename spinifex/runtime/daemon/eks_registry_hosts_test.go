package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/stretchr/testify/assert"
)

func registryTestNode(host, advertise, awsgwHost string) config.Config {
	n := config.Config{Host: host, AdvertiseIP: advertise}
	n.AWSGW.Host = awsgwHost
	return n
}

func TestRegistryHostsForCluster(t *testing.T) {
	issueCluster := func(localName string) *config.ClusterConfig {
		return &config.ClusterConfig{
			Node: localName,
			Nodes: map[string]config.Config{
				"node1": registryTestNode("192.168.1.228", "192.168.1.228", "0.0.0.0:9999"),
				"node2": registryTestNode("192.168.1.229", "192.168.1.229", ""),
				"node3": registryTestNode("192.168.1.230", "", ""),
				"node4": registryTestNode("192.168.1.231", "", ""),
			},
		}
	}

	tests := []struct {
		name  string
		cc    *config.ClusterConfig
		local config.Config
		want  []string
	}{
		{
			name:  "four-node cluster lists local first then peers by name",
			cc:    issueCluster("node3"),
			local: registryTestNode("192.168.1.230", "192.168.1.230", "0.0.0.0:9999"),
			want:  []string{"192.168.1.230:9999", "192.168.1.228:9999", "192.168.1.229:9999", "192.168.1.231:9999"},
		},
		{
			name:  "local concrete bind differing from advertise uses the bind",
			cc:    issueCluster("node1"),
			local: registryTestNode("192.168.1.228", "10.0.0.1", "192.168.1.250:9999"),
			want:  []string{"192.168.1.250:9999", "192.168.1.229:9999", "192.168.1.230:9999", "192.168.1.231:9999"},
		},
		{
			name: "peer with empty host is skipped",
			cc: &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{
				"node1": registryTestNode("10.0.0.1", "10.0.0.1", ""),
				"node2": registryTestNode("", "", ""),
				"node3": registryTestNode("10.0.0.3", "", ""),
			}},
			local: registryTestNode("10.0.0.1", "10.0.0.1", "0.0.0.0:9999"),
			want:  []string{"10.0.0.1:9999", "10.0.0.3:9999"},
		},
		{
			name: "local port 443 is omitted from the host",
			cc: &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{
				"node1": registryTestNode("10.0.0.1", "10.0.0.1", ""),
				"node2": registryTestNode("10.0.0.2", "", ""),
			}},
			local: registryTestNode("10.0.0.1", "10.0.0.1", "0.0.0.0:443"),
			want:  []string{"10.0.0.1", "10.0.0.2:9999"},
		},
		{
			name: "peers sharing a host are de-duplicated",
			cc: &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{
				"node1": registryTestNode("10.0.0.1", "10.0.0.1", ""),
				"node2": registryTestNode("10.0.0.2", "", ""),
				"node3": registryTestNode("10.0.0.2", "", ""),
			}},
			local: registryTestNode("10.0.0.1", "10.0.0.1", "0.0.0.0:9999"),
			want:  []string{"10.0.0.1:9999", "10.0.0.2:9999"},
		},
		{
			name: "local node with no concrete host contributes nothing",
			cc: &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{
				"node1": registryTestNode("", "", ""),
				"node2": registryTestNode("10.0.0.2", "", ""),
			}},
			local: registryTestNode("", "", "0.0.0.0:9999"),
			want:  []string{"10.0.0.2:9999"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, registryHostsForCluster(tt.cc, &tt.local))
		})
	}
}

func TestRegistryHostsForCluster_NilClusterConfig(t *testing.T) {
	local := registryTestNode("10.0.0.1", "10.0.0.1", "0.0.0.0:9999")
	assert.Equal(t, []string{"10.0.0.1:9999"}, registryHostsForCluster(nil, &local))
}
