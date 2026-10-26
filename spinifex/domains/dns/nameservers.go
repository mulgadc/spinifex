package dns

import (
	"fmt"
	"net"
	"sort"
	"strings"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
)

// NameserverSeeds derives the published nameservers (nsN → node IP) from the
// cluster nodes that run northstar and are reachable by their peers: the
// [northstar] pins first, then the rest in name order, capped at
// nameserver_count or MaxNameservers. Every node computes the same set. Falls
// back to the local node when none qualifies (single-node / dev).
//
// Both zone creators build from this: the northstar bootstrap seed and the
// writer's on-demand materialisation. Both are create-if-absent and neither
// overwrites, so they must agree — a set derived independently would be baked
// into the zone permanently by whichever creator ran first.
func NameserverSeeds(cluster *config.ClusterConfig) []nsconfig.NameserverSeed {
	names := publishedNameserverNodes(usableNorthstarNodeNames(cluster), cluster.Northstar)
	// A zone needs an SOA and at least one NS whatever the topology, so unlike the
	// resolver list this falls back to the local node. On single-node dev that node
	// is loopback, which is correct there: it is the zone's only reader.
	if len(names) == 0 {
		names = []string{cluster.Node}
	}

	seeds := make([]nsconfig.NameserverSeed, 0, len(names))
	for _, ns := range nameserverAssignments(cluster, names) {
		seeds = append(seeds, nsconfig.NameserverSeed{Host: ns.Host, IP: ns.IP})
	}
	return seeds
}

// Nameserver is one published nameserver: its label under the base domain,
// the node it is assigned to and that node's address.
type Nameserver struct {
	Host string
	Node string
	IP   string
}

// PublishedNameservers returns the nsN assignment NameserverSeeds publishes,
// with the node behind each name. Unlike NameserverSeeds it has no local-node
// fallback, so an empty result means no node qualifies.
func PublishedNameservers(cluster *config.ClusterConfig) []Nameserver {
	return nameserverAssignments(cluster, publishedNameserverNodes(usableNorthstarNodeNames(cluster), cluster.Northstar))
}

func nameserverAssignments(cluster *config.ClusterConfig, names []string) []Nameserver {
	out := make([]Nameserver, 0, len(names))
	for i, name := range names {
		out = append(out, Nameserver{Host: fmt.Sprintf("ns%d", i+1), Node: name, IP: nameserverIP(cluster.Nodes[name])})
	}
	return out
}

// ResolverNameserverIPs returns the WAN IPs of every cluster node running
// northstar in name order, not only the published nameservers. vpcd's per-tap DNS
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

// publishedNameserverNodes orders the usable northstar nodes for publication
// and truncates them to the configured count. A pinned node that is not usable
// is skipped rather than published with an address its peers cannot reach.
func publishedNameserverNodes(usable []string, ns config.NorthstarClusterConfig) []string {
	limit := ns.NameserverCount
	if limit <= 0 || limit > config.MaxNameservers {
		limit = config.MaxNameservers
	}
	ok := make(map[string]bool, len(usable))
	for _, name := range usable {
		ok[name] = true
	}
	ordered := make([]string, 0, len(usable))
	for _, name := range ns.NameserverNodes {
		if ok[name] {
			ordered = append(ordered, name)
			delete(ok, name)
		}
	}
	for _, name := range usable {
		if ok[name] {
			ordered = append(ordered, name)
		}
	}
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
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

// BaseZoneNameserverChanges returns the set-valued UPSERTs that hold the base
// zone's apex NS set and each nsK glue record to the published assignment, so a
// node that joins or leaves moves them without rebuilding the zone.
func BaseZoneNameserverChanges(base string, published []Nameserver) []Change {
	base = strings.TrimSuffix(strings.TrimSpace(base), ".")
	if base == "" || len(published) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(published))
	changes := make([]Change, 0, len(published)+1)
	for _, ns := range published {
		hosts = append(hosts, ns.Host+"."+base+".")
		changes = append(changes, Change{Action: ActionUpsertSet, Zone: base, Name: ns.Host + "." + base, Type: "A", Values: []string{ns.IP}})
	}
	return append(changes, Change{Action: ActionUpsertSet, Zone: base, Name: base, Type: "NS", Values: hosts})
}

// isNameserverGlueLabel reports whether a base-zone label is an nsK glue name.
func isNameserverGlueLabel(label string) bool {
	n := strings.TrimSuffix(strings.ToLower(label), ".")
	if len(n) < 3 || !strings.HasPrefix(n, "ns") {
		return false
	}
	for _, r := range n[2:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
