package formation

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/admin"
)

// ovnDBQuorum is the number of nodes that host the clustered OVN NB/SB
// databases. The first ovnDBQuorum nodes (by sorted name) form the RAFT
// quorum; every other node is OVN compute-only and dials these endpoints.
const ovnDBQuorum = 3

// OVN database client ports served by each RAFT quorum node.
const (
	ovnNBPort = 6641
	ovnSBPort = 6642
)

// hasService reports whether the node's service list includes name.
// An empty list means all services (backward compat).
func hasService(services []string, name string) bool {
	if len(services) == 0 {
		return true // backward compat: empty = all services
	}
	return slices.Contains(services, name)
}

// BuildClusterRoutes returns sorted "IP:4248" NATS cluster routes for nodes
// running the "nats" service. ClusterIP is preferred over BindIP.
func BuildClusterRoutes(nodes map[string]NodeInfo) []string {
	natsNodes := make(map[string]NodeInfo)
	for k, n := range nodes {
		if hasService(n.Services, "nats") {
			natsNodes[k] = n
		}
	}
	sorted := sortedNodes(natsNodes)
	routes := make([]string, len(sorted))
	for i, n := range sorted {
		ip := n.ClusterIP
		if ip == "" {
			ip = n.BindIP
		}
		routes[i] = ip + ":4248"
	}
	return routes
}

// BuildPredastoreNodes returns sorted PredastoreNodeConfig (1-based IDs) for
// nodes running the "predastore" service.
func BuildPredastoreNodes(nodes map[string]NodeInfo) []admin.PredastoreNodeConfig {
	predaNodes := make(map[string]NodeInfo)
	for k, n := range nodes {
		if hasService(n.Services, "predastore") {
			predaNodes[k] = n
		}
	}
	sorted := sortedNodes(predaNodes)
	out := make([]admin.PredastoreNodeConfig, len(sorted))
	for i, n := range sorted {
		out[i] = admin.PredastoreNodeConfig{
			ID:   i + 1,
			Host: n.BindIP,
		}
	}
	return out
}

// OVNDBQuorumNames returns the sorted names of the nodes that host the
// clustered OVN NB/SB databases: the first ovnDBQuorum names (sorted). Every
// other node is OVN compute-only and does not run the DB servers. Endpoint
// construction (BuildOVNDBAddrs) and role probing both route through this so
// they can never disagree on membership; callers test membership with
// slices.Contains.
func OVNDBQuorumNames(nodeNames []string) []string {
	sorted := slices.Sorted(slices.Values(nodeNames))
	if len(sorted) > ovnDBQuorum {
		sorted = sorted[:ovnDBQuorum]
	}
	return sorted
}

// BuildOVNDBAddrs returns comma-separated OVN NB and SB endpoint lists for the
// RAFT quorum nodes (first ovnDBQuorum by sorted name). Every node — quorum and
// compute alike — points its OVN client at the full list so libovsdb fails over
// across the cluster. BindIP is the cross-node dial address.
func BuildOVNDBAddrs(nodes map[string]NodeInfo) (nbAddr, sbAddr string) {
	members := OVNDBQuorumNames(slices.Collect(maps.Keys(nodes)))
	nb := make([]string, len(members))
	sb := make([]string, len(members))
	for i, name := range members {
		n := nodes[name]
		nb[i] = fmt.Sprintf("tcp:%s:%d", n.BindIP, ovnNBPort)
		sb[i] = fmt.Sprintf("tcp:%s:%d", n.BindIP, ovnSBPort)
	}
	return strings.Join(nb, ","), strings.Join(sb, ",")
}

// sortedNodes returns nodes sorted by name.
func sortedNodes(nodes map[string]NodeInfo) []NodeInfo {
	names := slices.Sorted(maps.Keys(nodes))

	sorted := make([]NodeInfo, len(names))
	for i, name := range names {
		sorted[i] = nodes[name]
	}
	return sorted
}
