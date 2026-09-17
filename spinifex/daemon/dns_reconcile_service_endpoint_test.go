//test:in-package — builds *Daemon literals with a populated clusterConfig to
//drive the service-endpoint desired-set builder directly.

package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A multi-node cluster's desired set must carry every node's own gateway
// address, not just the calling node's — whichever node's reconcile wins the
// leader election for a given cycle has to compute the identical full set, or
// the next cycle's winner would erase what a previous winner published.
func TestDesiredServiceEndpointDNSChanges_SpansEveryNode(t *testing.T) {
	d := &Daemon{
		mgmtBridgeIP: "10.15.8.1",
		config:       &config.Config{Node: "node1", Region: "ap-southeast-2"},
		clusterConfig: &config.ClusterConfig{
			Node: "node1",
			AWS:  config.AWSConfig{InternalSuffix: "spinifex.internal"},
			Nodes: map[string]config.Config{
				"node1": {Node: "node1", AdvertiseIP: "203.0.113.10"},
				"node2": {Node: "node2", AdvertiseIP: "203.0.113.20"},
			},
		},
	}

	changes, ok := d.desiredServiceEndpointDNSChanges()
	require.True(t, ok, "a readable cluster topology carries prune authority")
	require.NotEmpty(t, changes)
	for _, c := range changes {
		assert.ElementsMatch(t, []string{"203.0.113.10", "203.0.113.20"}, c.Values,
			"every service-endpoint change carries the full node address set")
	}
}

// The local node uses its own live-detected mgmt-bridge IP when nothing more
// specific is configured; peers only carry their gossiped AdvertiseIP.
func TestDesiredServiceEndpointDNSChanges_LocalNodeUsesLiveMgmtBridge(t *testing.T) {
	d := &Daemon{
		mgmtBridgeIP: "10.15.8.1",
		config:       &config.Config{Node: "node1", Region: "us-east-1"},
		clusterConfig: &config.ClusterConfig{
			Node: "node1",
			AWS:  config.AWSConfig{InternalSuffix: "spinifex.internal"},
			Nodes: map[string]config.Config{
				// No AdvertiseIP, no AWSGW host: only the live mgmt-bridge IP
				// (local-only) resolves this node's own address.
				"node1": {Node: "node1"},
			},
		},
	}

	changes, ok := d.desiredServiceEndpointDNSChanges()
	require.True(t, ok)
	require.NotEmpty(t, changes)
	assert.Equal(t, []string{"10.15.8.1"}, changes[0].Values)
}

// A non-IP host (AdvertiseIP/Host are free-form) must be skipped rather than
// published as a malformed A record — the writer supports only IP literals.
func TestDesiredServiceEndpointDNSChanges_SkipsNonIPHosts(t *testing.T) {
	d := &Daemon{
		config: &config.Config{Node: "node1", Region: "us-east-1"},
		clusterConfig: &config.ClusterConfig{
			Node: "node1",
			AWS:  config.AWSConfig{InternalSuffix: "spinifex.internal"},
			Nodes: map[string]config.Config{
				"node1": {Node: "node1", AdvertiseIP: "not-an-ip-literal"},
				"node2": {Node: "node2", AdvertiseIP: "203.0.113.20"},
			},
		},
	}

	changes, ok := d.desiredServiceEndpointDNSChanges()
	require.True(t, ok)
	require.NotEmpty(t, changes)
	assert.Equal(t, []string{"203.0.113.20"}, changes[0].Values,
		"the non-IP node is dropped rather than written as a malformed record")
}

// No suffix configured, or no cluster topology at all, must yield no changes
// and no authority — the reconciler must never prune this zone from a partial
// or absent view.
func TestDesiredServiceEndpointDNSChanges_NoAuthorityWithoutTopologyOrSuffix(t *testing.T) {
	cases := map[string]*Daemon{
		"nil cluster config": {config: &config.Config{}},
		"empty suffix": {
			config:        &config.Config{},
			clusterConfig: &config.ClusterConfig{Nodes: map[string]config.Config{}},
		},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			changes, ok := d.desiredServiceEndpointDNSChanges()
			assert.False(t, ok)
			assert.Empty(t, changes)
		})
	}
}

// The flag and the records travel together in the full desired set, the same
// contract EC2/ELB/EKS/RDS already hold.
func TestDNSDesiredSet_ServiceEndpointAuthorityFollowsTheRecords(t *testing.T) {
	d := &Daemon{
		config: &config.Config{Node: "node1", Region: "us-east-1"},
		clusterConfig: &config.ClusterConfig{
			Node: "node1",
			AWS:  config.AWSConfig{InternalSuffix: "spinifex.internal"},
			Nodes: map[string]config.Config{
				"node1": {Node: "node1", AdvertiseIP: "203.0.113.10"},
			},
		},
	}

	ds := d.dnsDesiredSet()
	assert.True(t, ds.Prunable.ServiceEndpoint)
	assert.NotEmpty(t, ds.Changes)

	assert.False(t, (&Daemon{config: &config.Config{}}).dnsDesiredSet().Prunable.ServiceEndpoint,
		"a daemon with no cluster config claims no service-endpoint authority")
}
