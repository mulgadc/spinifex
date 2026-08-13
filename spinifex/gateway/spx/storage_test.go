package spx

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetStorageStatus_Success(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	// Mock daemon returning storage config
	sub, err := nc.Subscribe("spinifex.storage.config", func(msg *nats.Msg) {
		resp := types.StorageConfigResponse{
			Encoding: types.StorageEncoding{DataShards: 2, ParityShards: 1},
			MetaNodes: []types.StorageMetaNode{
				{ID: 1, Host: "127.0.0.1", Port: 1}, // nothing listens on port 1
			},
			BlobNodes: []types.StorageBlobNode{
				{ID: 1, Host: "0.0.0.0", Port: 9991},
				{ID: 2, Host: "0.0.0.0", Port: 9992},
			},
			Buckets: []types.StorageBucket{
				{Name: "predastore", Region: "ap-southeast-2"},
			},
		}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	nc.Flush()

	out, err := GetStorageStatus(nc, nil)
	require.NoError(t, err)

	assert.Equal(t, "Reed-Solomon", out.Encoding.Type)
	assert.Equal(t, 2, out.Encoding.DataShards)
	assert.Equal(t, 1, out.Encoding.ParityShards)
	assert.Len(t, out.BlobNodes, 2)
	assert.Len(t, out.Buckets, 1)
	assert.Equal(t, "predastore", out.Buckets[0].Name)
	// Meta node status probe will fail (no real predastore running) — expected
	require.Len(t, out.MetaNodes, 1)
	assert.Equal(t, 1, out.MetaNodes[0].ID)
	assert.False(t, out.MetaNodes[0].Healthy)
}

func TestGetStorageStatus_NoNATSResponse(t *testing.T) {
	_, nc := startEmbeddedNATS(t)
	// No subscriber — should timeout
	_, err := GetStorageStatus(nc, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage config request")
}

func TestApplyMetaStatus_MapsRaftFields(t *testing.T) {
	out := &MetaNodeStatus{ID: 1, Host: "10.0.0.1", Port: 6100}
	applyMetaStatus(out, pds.Status{
		NodeID:       "1",
		State:        "Leader",
		Leader:       "node-1",
		LeaderAddr:   "",
		Term:         "42",
		CommitIndex:  "1000",
		AppliedIndex: "998",
		IsLeader:     true,
	})

	assert.True(t, out.Healthy)
	assert.Equal(t, "Leader", out.State)
	assert.Equal(t, "node-1", out.Leader)
	assert.True(t, out.IsLeader)
	assert.Equal(t, "42", out.Term)
	assert.Equal(t, "1000", out.CommitIdx)
	assert.Equal(t, "998", out.AppliedIdx)
}

func TestApplyMetaStatus_NoLeaderStaysHealthy(t *testing.T) {
	// A replica that answers but observes no leader is still a replica that
	// answered: Healthy describes reachability, not raft convergence.
	out := &MetaNodeStatus{ID: 2}
	applyMetaStatus(out, pds.Status{State: "Follower"})

	assert.True(t, out.Healthy)
	assert.Equal(t, "Follower", out.State)
	assert.Empty(t, out.Leader)
	assert.False(t, out.IsLeader)
}

func TestQueryMetaNodeStatus_Unreachable(t *testing.T) {
	out := &MetaNodeStatus{ID: 1, Host: "127.0.0.1", Port: 1} // nothing listens here
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	queryMetaNodeStatus(ctx, out, nil, "127.0.0.1", 1)

	assert.False(t, out.Healthy)
	assert.Empty(t, out.State)
}
