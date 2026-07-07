package gateway_ec2_capacityreservation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func censusFixture() []nodeCensus {
	return []nodeCensus{
		{NodeID: "node-a", AZ: "az-1", Available: map[string]int{"t3.micro": 4, "t3.small": 2}},
		{NodeID: "node-b", AZ: "az-1", Available: map[string]int{"t3.micro": 8}},
		{NodeID: "node-c", AZ: "az-2", Available: map[string]int{"t3.micro": 16}},
		{NodeID: "node-d", AZ: "az-1", Available: map[string]int{"t3.micro": 1}},
	}
}

// selectNode picks the highest-available node within the requested AZ.
func TestSelectNode_HighestAvailableInAZ(t *testing.T) {
	assert.Equal(t, "node-b", selectNode(censusFixture(), "az-1", "t3.micro", 2))
}

// Nodes outside the requested AZ are never considered.
func TestSelectNode_ExcludesOtherAZ(t *testing.T) {
	assert.Equal(t, "node-c", selectNode(censusFixture(), "az-2", "t3.micro", 10))
}

// A node qualifies only if it can fit the whole instance count.
func TestSelectNode_InsufficientCapacity(t *testing.T) {
	assert.Equal(t, "node-b", selectNode(censusFixture(), "az-1", "t3.micro", 5))
	assert.Empty(t, selectNode(censusFixture(), "az-1", "t3.micro", 9))
}

// Unknown type, unknown AZ, or empty census all yield no selection.
func TestSelectNode_NoEligibleNode(t *testing.T) {
	assert.Empty(t, selectNode(censusFixture(), "az-1", "g5.xlarge", 1))
	assert.Empty(t, selectNode(censusFixture(), "az-9", "t3.micro", 1))
	assert.Empty(t, selectNode(nil, "az-1", "t3.micro", 1))
}

// Equal-capacity nodes break ties by lowest node id for deterministic placement.
func TestSelectNode_TieBrokenByLowestNodeID(t *testing.T) {
	census := []nodeCensus{
		{NodeID: "node-z", AZ: "az-1", Available: map[string]int{"t3.micro": 4}},
		{NodeID: "node-a", AZ: "az-1", Available: map[string]int{"t3.micro": 4}},
	}
	assert.Equal(t, "node-a", selectNode(census, "az-1", "t3.micro", 1))
}

// collectCensus captures one entry per distinct node with its AZ and per-type
// available counts, completing as soon as expectedNodes have answered.
func TestCollectCensus_CapturesDistinctNodes(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	sub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		responses := []types.NodeStatusResponse{
			{Node: "node-1", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}}},
			{Node: "node-2", AZ: "az-2", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}}},
		}
		for _, r := range responses {
			data, _ := json.Marshal(r)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	census, err := collectCensus(context.Background(), nc, 2, "")
	require.NoError(t, err)
	require.Len(t, census, 2)

	byNode := make(map[string]nodeCensus, len(census))
	for _, c := range census {
		byNode[c.NodeID] = c
	}
	assert.Equal(t, "az-1", byNode["node-1"].AZ)
	assert.Equal(t, 4, byNode["node-1"].Available["t3.micro"])
	assert.Equal(t, "az-2", byNode["node-2"].AZ)
	assert.Equal(t, 2, byNode["node-2"].Available["t3.micro"])
}

// A node that answers more than once is recorded only once; the first reply wins.
func TestCollectCensus_DedupsRepeatNodes(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	sub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		responses := []types.NodeStatusResponse{
			{Node: "node-1", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}}},
			{Node: "node-1", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 99}}},
			{Node: "node-2", AZ: "az-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}}},
		}
		for _, r := range responses {
			data, _ := json.Marshal(r)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	// Gather counts frames, not distinct nodes: expect all 3 frames so the
	// caller's by-node dedup can collapse them to 2 distinct nodes.
	census, err := collectCensus(context.Background(), nc, 3, "")
	require.NoError(t, err)
	require.Len(t, census, 2)

	byNode := make(map[string]nodeCensus, len(census))
	for _, c := range census {
		byNode[c.NodeID] = c
	}
	assert.Equal(t, 4, byNode["node-1"].Available["t3.micro"], "first reply wins, duplicate ignored")
}
