package spx

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetVersion_OpenSource(t *testing.T) {
	out, err := GetVersion("v0.5.0-43-gae1deb5", "ae1deb5")
	require.NoError(t, err)
	assert.Equal(t, "v0.5.0-43-gae1deb5", out.Version)
	assert.Equal(t, "ae1deb5", out.Commit)
	assert.Equal(t, "open-source", out.License)
	assert.NotEmpty(t, out.OS)
	assert.NotEmpty(t, out.Arch)
}

func TestGetVersion_Commercial(t *testing.T) {
	out, err := GetVersion("v1.0.0 [commercial]", "abc1234")
	require.NoError(t, err)
	assert.Equal(t, "commercial", out.License)
}

func TestGetVersion_Dev(t *testing.T) {
	out, err := GetVersion("dev", "unknown")
	require.NoError(t, err)
	assert.Equal(t, "dev", out.Version)
	assert.Equal(t, "open-source", out.License)
}

// startEmbeddedNATS starts an in-process NATS server for testing.
func startEmbeddedNATS(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	return testutil.StartTestNATS(t)
}

func TestGetNodes_NoResponders(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	out, err := GetNodes(nc, 0)
	require.NoError(t, err)
	assert.Empty(t, out.Nodes)
	assert.Equal(t, "single-node", out.ClusterMode)
}

func TestGetNodes_SingleNode(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	sub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		resp := types.NodeStatusResponse{
			Node:       "node1",
			Status:     "Ready",
			Host:       "10.0.0.1",
			Region:     "ap-southeast-2",
			AZ:         "ap-southeast-2a",
			TotalVCPU:  8,
			TotalMemGB: 16.0,
			AllocVCPU:  2,
			AllocMemGB: 2.0,
			InstanceTypes: []types.InstanceTypeCap{
				{Name: "t3.small", VCPU: 2, MemoryGB: 2.0, Available: 3},
			},
		}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	nc.Flush()

	out, err := GetNodes(nc, 1)
	require.NoError(t, err)
	require.Len(t, out.Nodes, 1)
	assert.Equal(t, "node1", out.Nodes[0].Node)
	assert.Equal(t, "Ready", out.Nodes[0].Status)
	assert.Equal(t, 8, out.Nodes[0].TotalVCPU)
	assert.Len(t, out.Nodes[0].InstanceTypes, 1)
	assert.Equal(t, "t3.small", out.Nodes[0].InstanceTypes[0].Name)
	assert.Equal(t, 3, out.Nodes[0].InstanceTypes[0].Available)
	assert.Equal(t, "single-node", out.ClusterMode)
}

func TestGetNodes_MultiNode(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	for _, name := range []string{"node1", "node2", "node3"} {
		nodeName := name
		sub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
			resp := types.NodeStatusResponse{Node: nodeName, Status: "Ready"}
			data, _ := json.Marshal(resp)
			msg.Respond(data)
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()
	}
	nc.Flush()

	out, err := GetNodes(nc, 3)
	require.NoError(t, err)
	require.Len(t, out.Nodes, 3)
	assert.Equal(t, "multi-node", out.ClusterMode)
}

func TestGetVMs_NoResponders(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	out, err := GetVMs(nc, 0)
	require.NoError(t, err)
	assert.Empty(t, out.VMs)
}

func TestGetVMs_WithVMs(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	sub, err := nc.Subscribe("spinifex.node.vms", func(msg *nats.Msg) {
		resp := types.NodeVMsResponse{
			Node: "node1",
			Host: "10.0.0.1",
			VMs: []types.VMInfo{
				{InstanceID: "i-abc123", Status: "running", InstanceType: "t3.small"},
				{InstanceID: "i-def456", Status: "running", InstanceType: "t3.medium"},
			},
		}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	nc.Flush()

	out, err := GetVMs(nc, 1)
	require.NoError(t, err)
	require.Len(t, out.VMs, 2)
	assert.Equal(t, "i-abc123", out.VMs[0].InstanceID)
	assert.Equal(t, "node1", out.VMs[0].Node)
	assert.Equal(t, "i-def456", out.VMs[1].InstanceID)
	assert.Equal(t, "node1", out.VMs[1].Node)
}

func TestGetVMs_MultiNode(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	for _, name := range []string{"node1", "node2"} {
		nodeName := name
		sub, err := nc.Subscribe("spinifex.node.vms", func(msg *nats.Msg) {
			resp := types.NodeVMsResponse{
				Node: nodeName,
				VMs: []types.VMInfo{
					{InstanceID: "i-" + nodeName, Status: "running"},
				},
			}
			data, _ := json.Marshal(resp)
			msg.Respond(data)
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()
	}
	nc.Flush()

	out, err := GetVMs(nc, 2)
	require.NoError(t, err)
	require.Len(t, out.VMs, 2)
}
