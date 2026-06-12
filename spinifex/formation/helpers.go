package formation

import (
	"slices"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/admin"
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

// sortedNodes returns nodes sorted by name.
func sortedNodes(nodes map[string]NodeInfo) []NodeInfo {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	sorted := make([]NodeInfo, len(names))
	for i, name := range names {
		sorted[i] = nodes[name]
	}
	return sorted
}
