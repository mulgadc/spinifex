package dns

import (
	"fmt"
	"net"
	"sort"
	"strings"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
)

// NameserverSeeds derives one nameserver (nsN → node IP) per cluster node that
// runs northstar and is reachable by its peers, ordered deterministically so
// every node computes the same set. Falls back to the local node when none
// qualifies (single-node / dev).
//
// Both zone creators build from this: the northstar bootstrap seed and the
// writer's on-demand materialisation. Both are create-if-absent and neither
// overwrites, so they must agree — a set derived independently would be baked
// into the zone permanently by whichever creator ran first.
func NameserverSeeds(cluster *config.ClusterConfig) []nsconfig.NameserverSeed {
	names := usableNorthstarNodeNames(cluster)
	// A zone needs an SOA and at least one NS whatever the topology, so unlike the
	// resolver list this falls back to the local node. On single-node dev that node
	// is loopback, which is correct there: it is the zone's only reader.
	if len(names) == 0 {
		names = []string{cluster.Node}
	}

	seeds := make([]nsconfig.NameserverSeed, 0, len(names))
	for i, name := range names {
		seeds = append(seeds, nsconfig.NameserverSeed{
			Host: fmt.Sprintf("ns%d", i+1),
			IP:   nameserverIP(cluster.Nodes[name]),
		})
	}
	return seeds
}

// ResolverNameserverIPs returns the WAN IPs of cluster nodes running northstar,
// in the same deterministic order as the seeded nameservers. vpcd's per-tap DNS
// shim uses these as forward targets (northstar's :5300 listener), so internal
// names resolve authoritatively and external names via upstream forwarders.
//
// Unlike NameserverSeeds this has no local-node fallback: an empty list is the
// meaningful signal that the cluster has no usable DNS backend, and the caller
// falls back to the upstream pool DNS rather than advertising northstar.
func ResolverNameserverIPs(cluster *config.ClusterConfig) []string {
	names := usableNorthstarNodeNames(cluster)
	ips := make([]string, 0, len(names))
	for _, name := range names {
		ips = append(ips, nameserverIP(cluster.Nodes[name]))
	}
	return ips
}

// usableNorthstarNodeNames returns the deterministically ordered nodes that run
// northstar and can be reached by their peers.
//
// Loopback is excluded: a node that could not detect a WAN IP advertises
// 127.0.0.1, which means "this node" to whoever reads it. Published as zone glue
// it would send every peer's resolver to its own loopback, so such a node must
// drop out of the seed set and the resolver list together.
func usableNorthstarNodeNames(cluster *config.ClusterConfig) []string {
	names := configuredNorthstarNodeNames(cluster)
	usable := make([]string, 0, len(names))
	for _, name := range names {
		if isLoopbackAddr(nameserverIP(cluster.Nodes[name])) {
			continue
		}
		usable = append(usable, name)
	}
	return usable
}

// configuredNorthstarNodeNames returns the deterministically ordered nodes that
// explicitly advertise a northstar configuration.
func configuredNorthstarNodeNames(cluster *config.ClusterConfig) []string {
	var names []string
	for name, node := range cluster.Nodes {
		if node.Northstar.ConfigPath != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// nameserverIP returns a reachable DNS address for a node: its advertised IP,
// else its host, with any port stripped. A missing or wildcard address falls
// back to loopback for single-node/dev.
func nameserverIP(node config.Config) string {
	ip := strings.TrimSpace(node.AdvertiseIP)
	if ip == "" {
		ip = strings.TrimSpace(node.Host)
	}
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if ip == "" || ip == "0.0.0.0" {
		ip = "127.0.0.1"
	}
	return ip
}

// isLoopbackAddr reports whether an address is a loopback literal. Hostnames are
// not resolved, so they count as usable.
func isLoopbackAddr(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}
