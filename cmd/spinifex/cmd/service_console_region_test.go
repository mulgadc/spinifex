package cmd_test

import (
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsoleRegion_ReadsTheServingNode(t *testing.T) {
	cfg := &config.ClusterConfig{
		Node: "node2",
		Nodes: map[string]config.Config{
			"node1": {Region: "us-east-1"},
			"node2": {Region: "us-west-1"},
		},
	}

	region, err := cmd.ConsoleRegion(cfg)

	require.NoError(t, err)
	assert.Equal(t, "us-west-1", region)
}

// Serving a default would sign with a region awsgw does not verify against.
func TestConsoleRegion_FailsWhenNodeHasNoRegion(t *testing.T) {
	cfg := &config.ClusterConfig{
		Node:  "node1",
		Nodes: map[string]config.Config{"node1": {}},
	}

	_, err := cmd.ConsoleRegion(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "node1")
}

func TestConsoleRegion_FailsWhenNodeIsAbsent(t *testing.T) {
	_, err := cmd.ConsoleRegion(&config.ClusterConfig{Node: "ghost"})

	require.Error(t, err)
}
