package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A peer's section carries only host, so the cluster's service addresses come
// from it; a bind-all, loopback or hostname host names no address.
func TestServiceEndpoints_PeersFallBackToHost(t *testing.T) {
	cc := &ClusterConfig{Node: "node1", Nodes: map[string]Config{
		"node1": {Node: "node1", Host: "192.168.1.26", AdvertiseIP: "192.168.1.26", AWSGW: AWSGWConfig{Host: "0.0.0.0:9999"}},
		"node2": {Node: "node2", Host: "192.168.1.27"},
		"node3": {Node: "node3", Host: "192.168.1.28"},
		"node4": {Node: "node4", Host: "0.0.0.0"},
		"node5": {Node: "node5", Host: "127.0.0.1"},
		"node6": {Node: "node6", Host: "node6.example"},
	}}
	var got []string
	for _, ep := range ServiceEndpoints(cc, DefaultMgmtBridgeIP) {
		got = append(got, ep.Node+"="+ep.Addr.String())
	}
	assert.Equal(t, []string{"node1=192.168.1.26", "node2=192.168.1.27", "node3=192.168.1.28"}, got)
}

// Host is the last resort: an advertise address or the local br-mgmt address wins.
func TestGatewayHostForNode_HostIsTheLastResort(t *testing.T) {
	assert.Equal(t, "10.0.0.5", GatewayHostForNode(Config{Host: "10.0.0.9", AdvertiseIP: "10.0.0.5"}, ""))
	assert.Equal(t, DefaultMgmtBridgeIP, GatewayHostForNode(Config{Host: "10.0.0.9"}, DefaultMgmtBridgeIP))
	assert.Equal(t, "10.0.0.9", GatewayHostForNode(Config{Host: "10.0.0.9"}, ""))
}
