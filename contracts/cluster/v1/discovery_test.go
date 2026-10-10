package clusterv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeDiscoverResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(NodeDiscoverResponse{Node: "node-a"})
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-a"}`, string(data))
}

func TestNodesDiscoverSubject(t *testing.T) {
	require.Equal(t, "spinifex.nodes.discover", NodesDiscoverSubject)
}
