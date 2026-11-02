package dns

import (
	"fmt"
	"testing"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNameserverIP(t *testing.T) {
	assert.Equal(t, "10.11.12.1", nameserverIP(config.Config{AdvertiseIP: "10.11.12.1"}))
	assert.Equal(t, "10.11.12.2", nameserverIP(config.Config{Host: "10.11.12.2:8443"}))
	assert.Equal(t, "10.0.0.9", nameserverIP(config.Config{AdvertiseIP: "10.0.0.9", Host: "10.0.0.1"}))
	assert.Equal(t, "127.0.0.1", nameserverIP(config.Config{Host: "0.0.0.0"}))
	assert.Equal(t, "127.0.0.1", nameserverIP(config.Config{}))
}

func TestNameserverSeeds(t *testing.T) {
	// Single node with no northstar config path → fall back to the local node.
	single := &config.ClusterConfig{
		Node:  "node1",
		Nodes: map[string]config.Config{"node1": {Host: "0.0.0.0"}},
	}
	seeds := NameserverSeeds(single)
	require.Len(t, seeds, 1)
	assert.Equal(t, "ns1", seeds[0].Host)
	assert.Equal(t, "127.0.0.1", seeds[0].IP)

	// Multi-node: one nameserver per node that advertises a northstar config,
	// ordered deterministically.
	multi := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node2": {AdvertiseIP: "10.0.0.2", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
			"node1": {AdvertiseIP: "10.0.0.1", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
			"node3": {AdvertiseIP: "10.0.0.3"}, // no northstar → excluded
		},
	}
	seeds = NameserverSeeds(multi)
	require.Len(t, seeds, 2)
	assert.Equal(t, "ns1", seeds[0].Host)
	assert.Equal(t, "10.0.0.1", seeds[0].IP)
	assert.Equal(t, "ns2", seeds[1].Host)
	assert.Equal(t, "10.0.0.2", seeds[1].IP)
}

// Every node must derive the same seed set, so the zone's NS records do not
// depend on which node wins the create-if-absent race.
func TestNameserverSeedsIdenticalFromEveryNode(t *testing.T) {
	nodes := map[string]config.Config{
		"node1": {AdvertiseIP: "10.0.0.1", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
		"node2": {AdvertiseIP: "10.0.0.2", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
		"node3": {AdvertiseIP: "10.0.0.3", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
	}
	want := NameserverSeeds(&config.ClusterConfig{Node: "node1", Nodes: nodes})
	require.Len(t, want, 3)
	for _, local := range []string{"node2", "node3"} {
		assert.Equal(t, want, NameserverSeeds(&config.ClusterConfig{Node: local, Nodes: nodes}),
			"seed set must not depend on which node derives it")
	}
}

// A node whose WAN detection failed advertises loopback. It must drop out of the
// cluster-wide zone, not publish nsN A 127.0.0.1 glue that every peer would
// follow to its own loopback, and the surviving nodes must still seed a zone.
func TestNameserverSeedsExcludeLoopbackNodes(t *testing.T) {
	nodes := map[string]config.Config{
		"node1": {AdvertiseIP: "10.0.0.1", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
		"node2": {AdvertiseIP: "127.0.0.1", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
		"node3": {AdvertiseIP: "10.0.0.3", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
		"node4": {AdvertiseIP: "::1", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
	}
	seeds := NameserverSeeds(&config.ClusterConfig{Node: "node1", Nodes: nodes})
	require.Len(t, seeds, 2)
	assert.Equal(t, "ns1", seeds[0].Host)
	assert.Equal(t, "10.0.0.1", seeds[0].IP)
	assert.Equal(t, "ns2", seeds[1].Host)
	assert.Equal(t, "10.0.0.3", seeds[1].IP)

	// Excluding a node must not fork the seed set: the loopback node derives the
	// same records as its peers, including from its own point of view.
	assert.Equal(t, seeds, NameserverSeeds(&config.ClusterConfig{Node: "node2", Nodes: nodes}))

	assert.Equal(t, []string{"10.0.0.1", "10.0.0.3"},
		ResolverNameserverIPs(&config.ClusterConfig{Node: "node1", Nodes: nodes}))
}

// The case the local-node fallback exists for: on single-node dev the only node
// is loopback, so the filter empties the set. Without the fallback the zone would
// be created with no SOA, no NS and no glue — and never overwritten.
func TestNameserverSeedsSingleNodeLoopbackFallback(t *testing.T) {
	dev := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{
		"node1": {Host: "0.0.0.0", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
	}}
	seeds := NameserverSeeds(dev)
	require.Len(t, seeds, 1)
	assert.Equal(t, "ns1", seeds[0].Host)
	assert.Equal(t, "127.0.0.1", seeds[0].IP)

	// The zone is seeded, but no guest is pointed at that loopback.
	assert.Empty(t, ResolverNameserverIPs(dev))
}

func TestResolverNameserverIPs(t *testing.T) {
	// Multi-node: WAN IPs of the northstar nodes, ordered, non-northstar excluded.
	multi := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node2": {AdvertiseIP: "192.168.1.32", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
			"node1": {AdvertiseIP: "192.168.1.31", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
			"node3": {AdvertiseIP: "192.168.1.33"}, // no northstar → excluded
		},
	}
	assert.Equal(t, []string{"192.168.1.31", "192.168.1.32"}, ResolverNameserverIPs(multi))

	// Resolver discovery must not reuse the bootstrap fallback: a reachable node
	// without northstar configured is not a valid DNS backend.
	disabled := &config.ClusterConfig{
		Node: "node1",
		Nodes: map[string]config.Config{
			"node1": {AdvertiseIP: "192.168.1.31"},
			"node2": {AdvertiseIP: "192.168.1.32"},
		},
	}
	assert.Empty(t, ResolverNameserverIPs(disabled))

	// Loopback-only nodes are also excluded so the caller falls back to the
	// upstream pool DNS.
	dev := &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{
		"node1": {Host: "0.0.0.0", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}},
	}}
	assert.Empty(t, ResolverNameserverIPs(dev))
}

func northstarNodes(n int) map[string]config.Config {
	nodes := make(map[string]config.Config, n)
	for i := 1; i <= n; i++ {
		nodes[fmt.Sprintf("node%02d", i)] = config.Config{
			AdvertiseIP: fmt.Sprintf("10.0.0.%d", i),
			Northstar:   config.NorthstarConfig{ConfigPath: "/etc/n.toml"},
		}
	}
	return nodes
}

func seedIPs(seeds []nsconfig.NameserverSeed) []string {
	ips := make([]string, 0, len(seeds))
	for i, s := range seeds {
		if s.Host != fmt.Sprintf("ns%d", i+1) {
			return []string{"misnumbered " + s.Host}
		}
		ips = append(ips, s.IP)
	}
	return ips
}

// A large cluster publishes at most four nameservers, while every northstar
// node stays a resolver backend for the guest shim.
func TestNameserverSeedsCappedAtFour(t *testing.T) {
	cluster := &config.ClusterConfig{Node: "node01", Nodes: northstarNodes(12)}
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}, seedIPs(NameserverSeeds(cluster)))
	assert.Len(t, ResolverNameserverIPs(cluster), 12)
}

func TestNameserverSeedsHonourCountAndPins(t *testing.T) {
	nodes := northstarNodes(6)
	nodes["node06"] = config.Config{AdvertiseIP: "127.0.0.1", Northstar: config.NorthstarConfig{ConfigPath: "/etc/n.toml"}}

	for name, tc := range map[string]struct {
		ns   config.NorthstarClusterConfig
		want []string
	}{
		"count below the node count": {config.NorthstarClusterConfig{NameserverCount: 2}, []string{"10.0.0.1", "10.0.0.2"}},
		"pins first, rest in name order": {
			config.NorthstarClusterConfig{NameserverNodes: []string{"node05", "node03"}},
			[]string{"10.0.0.5", "10.0.0.3", "10.0.0.1", "10.0.0.2"},
		},
		"pins fill the count": {
			config.NorthstarClusterConfig{NameserverCount: 2, NameserverNodes: []string{"node04", "node02"}},
			[]string{"10.0.0.4", "10.0.0.2"},
		},
		"unusable pin is skipped": {
			config.NorthstarClusterConfig{NameserverCount: 2, NameserverNodes: []string{"node06", "node05"}},
			[]string{"10.0.0.5", "10.0.0.1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cluster := &config.ClusterConfig{Node: "node01", Nodes: nodes, Northstar: tc.ns}
			assert.Equal(t, tc.want, seedIPs(NameserverSeeds(cluster)))
		})
	}
}
