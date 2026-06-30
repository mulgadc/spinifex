package formation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasService_EmptyListMeansAll(t *testing.T) {
	t.Parallel()
	assert.True(t, hasService(nil, "nats"))
	assert.True(t, hasService([]string{}, "predastore"))
}

func TestHasService_ExplicitList(t *testing.T) {
	t.Parallel()
	services := []string{"nats", "daemon"}
	assert.True(t, hasService(services, "nats"))
	assert.True(t, hasService(services, "daemon"))
	assert.False(t, hasService(services, "predastore"))
}

func TestBuildClusterRoutes_FiltersNonNATSNodes(t *testing.T) {
	t.Parallel()
	nodes := map[string]NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1", Services: []string{"nats", "daemon"}},
		"node2": {Name: "node2", BindIP: "10.0.0.2", Services: []string{"daemon"}}, // no NATS
		"node3": {Name: "node3", BindIP: "10.0.0.3", Services: []string{"nats", "predastore"}},
	}

	routes := BuildClusterRoutes(nodes)
	assert.Equal(t, []string{"10.0.0.1:4248", "10.0.0.3:4248"}, routes)
}

func TestBuildClusterRoutes_EmptyServicesIncludesAll(t *testing.T) {
	t.Parallel()
	// Nodes with empty services (backward compat) should all be included
	nodes := map[string]NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1"},
		"node2": {Name: "node2", BindIP: "10.0.0.2"},
	}

	routes := BuildClusterRoutes(nodes)
	assert.Equal(t, []string{"10.0.0.1:4248", "10.0.0.2:4248"}, routes)
}

func TestBuildPredastoreNodes_FiltersByPredastore(t *testing.T) {
	t.Parallel()
	nodes := map[string]NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1", Services: []string{"nats", "predastore"}},
		"node2": {Name: "node2", BindIP: "10.0.0.2", Services: []string{"daemon"}}, // no predastore
		"node3": {Name: "node3", BindIP: "10.0.0.3", Services: []string{"predastore"}},
	}

	pnodes := BuildPredastoreNodes(nodes)
	require.Len(t, pnodes, 2)
	assert.Equal(t, 1, pnodes[0].ID)
	assert.Equal(t, "10.0.0.1", pnodes[0].Host)
	assert.Equal(t, 2, pnodes[1].ID)
	assert.Equal(t, "10.0.0.3", pnodes[1].Host)
}

func TestBuildPredastoreNodes_EmptyServicesIncludesAll(t *testing.T) {
	t.Parallel()
	nodes := map[string]NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1"},
		"node2": {Name: "node2", BindIP: "10.0.0.2"},
		"node3": {Name: "node3", BindIP: "10.0.0.3"},
	}

	pnodes := BuildPredastoreNodes(nodes)
	require.Len(t, pnodes, 3)
}

func TestBuildOVNDBAddrs_CapsAtQuorum(t *testing.T) {
	t.Parallel()
	nodes := map[string]NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1"},
		"node2": {Name: "node2", BindIP: "10.0.0.2"},
		"node3": {Name: "node3", BindIP: "10.0.0.3"},
		"node4": {Name: "node4", BindIP: "10.0.0.4"},
	}

	nb, sb := BuildOVNDBAddrs(nodes)
	assert.Equal(t, "tcp:10.0.0.1:6641,tcp:10.0.0.2:6641,tcp:10.0.0.3:6641", nb)
	assert.Equal(t, "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642,tcp:10.0.0.3:6642", sb)
}

func TestBuildOVNDBAddrs_FewerThanQuorum(t *testing.T) {
	t.Parallel()
	nodes := map[string]NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1"},
		"node2": {Name: "node2", BindIP: "10.0.0.2"},
	}

	nb, sb := BuildOVNDBAddrs(nodes)
	assert.Equal(t, "tcp:10.0.0.1:6641,tcp:10.0.0.2:6641", nb)
	assert.Equal(t, "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642", sb)
}

func TestBuildClusterRoutes_MixedServicesAndEmpty(t *testing.T) {
	t.Parallel()
	nodes := map[string]NodeInfo{
		"node1": {Name: "node1", BindIP: "10.0.0.1"},                               // empty = all
		"node2": {Name: "node2", BindIP: "10.0.0.2", Services: []string{"daemon"}}, // no nats
		"node3": {Name: "node3", BindIP: "10.0.0.3", Services: []string{"nats"}},   // has nats
	}

	routes := BuildClusterRoutes(nodes)
	// node1 (empty=all, included), node2 (no nats, excluded), node3 (has nats, included)
	assert.Equal(t, []string{"10.0.0.1:4248", "10.0.0.3:4248"}, routes)
}
