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
// runs northstar, ordered deterministically so every node computes the same set.
// Falls back to the local node when no node advertises a northstar config
// (single-node / dev).
//
// Both zone creators build from this: the northstar bootstrap seed and the
// writer's on-demand materialisation. Both are create-if-absent and neither
// overwrites, so they must agree — a set derived independently would be baked
// into the zone permanently by whichever creator ran first.
func NameserverSeeds(cluster *config.ClusterConfig) []nsconfig.NameserverSeed {
	names := configuredNorthstarNodeNames(cluster)
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
// Loopback is skipped: a dev/misconfig node with no reachable IP yields an empty
// list, letting the caller fall back to the upstream pool DNS.
//
// Unlike NameserverSeeds this has no local-node fallback: a node that does not
// run northstar is not a valid DNS backend, however reachable it is.
func ResolverNameserverIPs(cluster *config.ClusterConfig) []string {
	names := configuredNorthstarNodeNames(cluster)
	ips := make([]string, 0, len(names))
	for _, name := range names {
		ip := nameserverIP(cluster.Nodes[name])
		if ip != "" && ip != "127.0.0.1" {
			ips = append(ips, ip)
		}
	}
	return ips
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
