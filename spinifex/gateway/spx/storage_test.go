package spx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pds "github.com/mulgadc/predastore"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetStorageStatus_Success(t *testing.T) {
	_, nc := startEmbeddedNATS(t)

	// Mock daemon returning storage config
	sub, err := nc.Subscribe(operatorv1.StorageConfigSubject, func(msg *nats.Msg) {
		resp := operatorv1.StorageConfigResponse{
			Encoding: operatorv1.StorageEncoding{DataShards: 2, ParityShards: 1},
			MetaNodes: []operatorv1.StorageMetaNode{
				{ID: 1, Host: "127.0.0.1", Port: 1}, // nothing listens on port 1
			},
			BlobNodes: []operatorv1.StorageBlobNode{
				{ID: 1, Host: "0.0.0.0", Port: 9991},
				{ID: 2, Host: "0.0.0.0", Port: 9992},
			},
			Buckets: []operatorv1.StorageBucket{
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

// readyzServer stands in for a predastore admin listener whose /readyz
// answers code with body.
func readyzServer(t *testing.T, code int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// serveStorageConfig answers spinifex.storage.config with resp, as the daemon
// would, and returns the gateway's report.
func serveStorageConfig(t *testing.T, resp operatorv1.StorageConfigResponse) *StorageStatusOutput {
	t.Helper()
	_, nc := startEmbeddedNATS(t)
	sub, err := nc.Subscribe("spinifex.storage.config", func(msg *nats.Msg) {
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	out, err := GetStorageStatus(nc, nil)
	require.NoError(t, err)
	require.Len(t, out.MetaNodes, 1)
	return out
}

// singleHostMeta is the default install's meta node: nothing listens on its
// QUIC port, so its health can only come from the admin listener at addr.
func singleHostMeta(addr string) operatorv1.StorageConfigResponse {
	return operatorv1.StorageConfigResponse{
		MetaNodes: []operatorv1.StorageMetaNode{{ID: 3, Host: "127.0.0.1", Port: 1, AdminAddr: addr}},
	}
}

func TestGetStorageStatus_SingleHostReadsReadyz(t *testing.T) {
	addr := readyzServer(t, http.StatusOK,
		`{"checks":{"blob_nodes":"ok","meta_leader":"ok","meta_reachable":"ok"},"status":"ready"}`)

	node := serveStorageConfig(t, singleHostMeta(addr)).MetaNodes[0]

	assert.True(t, node.Healthy)
	assert.Empty(t, node.State, "/readyz carries no raft detail to report")
	assert.Empty(t, node.Leader)
}

// /readyz answers 503 when any check fails; a blob failure must not mark a
// meta node that observes a leader unreachable.
func TestGetStorageStatus_ReadyzBlobFailureKeepsMetaHealthy(t *testing.T) {
	addr := readyzServer(t, http.StatusServiceUnavailable,
		`{"checks":{"blob_nodes":"fewer than 1 blob nodes answered","meta_leader":"ok"},"status":"unready"}`)

	assert.True(t, serveStorageConfig(t, singleHostMeta(addr)).MetaNodes[0].Healthy)
}

func TestGetStorageStatus_ReadyzWithoutLeaderStaysUnhealthy(t *testing.T) {
	addr := readyzServer(t, http.StatusServiceUnavailable,
		`{"checks":{"meta_leader":"no raft leader observed"},"status":"unready"}`)

	assert.False(t, serveStorageConfig(t, singleHostMeta(addr)).MetaNodes[0].Healthy)
}

// Before the sampler's first cycle /readyz names no checks at all.
func TestGetStorageStatus_ReadyzBeforeFirstSampleStaysUnhealthy(t *testing.T) {
	addr := readyzServer(t, http.StatusServiceUnavailable, `{"checks":{},"status":"unready"}`)

	assert.False(t, serveStorageConfig(t, singleHostMeta(addr)).MetaNodes[0].Healthy)
}

func TestQueryMetaNodeReadyz_Unreachable(t *testing.T) {
	out := &MetaNodeStatus{ID: 3}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	queryMetaNodeReadyz(ctx, out, "127.0.0.1:1")

	assert.False(t, out.Healthy)
}
