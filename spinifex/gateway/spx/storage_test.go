package spx

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

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
				{ID: 1, Host: "127.0.0.1", Port: 0}, // port set below
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

	out, err := GetStorageStatus(nc)
	require.NoError(t, err)

	assert.Equal(t, "Reed-Solomon", out.Encoding.Type)
	assert.Equal(t, 2, out.Encoding.DataShards)
	assert.Equal(t, 1, out.Encoding.ParityShards)
	assert.Len(t, out.BlobNodes, 2)
	assert.Len(t, out.Buckets, 1)
	assert.Equal(t, "predastore", out.Buckets[0].Name)
	// Meta node health check will fail (no real predastore running) — expected
	require.Len(t, out.MetaNodes, 1)
	assert.Equal(t, 1, out.MetaNodes[0].ID)
	assert.False(t, out.MetaNodes[0].Healthy) // no predastore server to respond
}

func TestGetStorageStatus_NoNATSResponse(t *testing.T) {
	_, nc := startEmbeddedNATS(t)
	// No subscriber — should timeout
	_, err := GetStorageStatus(nc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage config request")
}

func TestQueryMetaNodeStatus_HealthyNode(t *testing.T) {
	// Start a mock predastore meta node server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"status": "healthy"})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(predastoreStatusResponse{
			NodeID:     "1",
			State:      "Leader",
			Leader:     "1",
			LeaderAddr: "127.0.0.1:7660",
			Term:       "42",
			CommitIdx:  "1000",
			AppliedIdx: "998",
			IsLeader:   true,
		})
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	// Use the test server's TLS client
	origClient := storageHTTPClient
	storageHTTPClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	defer func() { storageHTTPClient = origClient }()

	// Parse host:port from the test server URL
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	out := &MetaNodeStatus{ID: 1, Host: host, Port: port}
	queryMetaNodeStatus(t.Context(), out, host, port)

	assert.True(t, out.Healthy)
	assert.Equal(t, "Leader", out.State)
	assert.True(t, out.IsLeader)
	assert.Equal(t, "42", out.Term)
	assert.Equal(t, "1000", out.CommitIdx)
	assert.Equal(t, "998", out.AppliedIdx)
}

func TestQueryMetaNodeStatus_UnreachableNode(t *testing.T) {
	out := &MetaNodeStatus{ID: 1, Host: "127.0.0.1", Port: 1} // port 1 won't respond
	queryMetaNodeStatus(t.Context(), out, "127.0.0.1", 1)

	assert.False(t, out.Healthy)
	assert.Empty(t, out.State)
}
