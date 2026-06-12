package spx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

// DBNodeStatus is a single DB node's live status merged with its config.
type DBNodeStatus struct {
	ID         int    `json:"id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Healthy    bool   `json:"healthy"`
	State      string `json:"state,omitempty"`
	Leader     string `json:"leader,omitempty"`
	LeaderAddr string `json:"leader_addr,omitempty"`
	Term       string `json:"term,omitempty"`
	CommitIdx  string `json:"commit_index,omitempty"`
	AppliedIdx string `json:"applied_index,omitempty"`
	IsLeader   bool   `json:"is_leader"`
}

// StorageStatusOutput is the response for GetStorageStatus.
type StorageStatusOutput struct {
	Encoding   StorageEncodingOutput    `json:"encoding"`
	DBNodes    []DBNodeStatus           `json:"db_nodes"`
	ShardNodes []types.StorageShardNode `json:"shard_nodes"`
	Buckets    []types.StorageBucket    `json:"buckets"`
}

// StorageEncodingOutput adds the type label to the encoding config.
type StorageEncodingOutput struct {
	Type         string `json:"type"`
	DataShards   int    `json:"data_shards"`
	ParityShards int    `json:"parity_shards"`
}

var storageHTTPClient = &http.Client{
	Timeout: 1 * time.Second,
}

// GetStorageStatus fetches predastore topology via NATS, then queries each DB
// node's /status and /health endpoints in parallel.
func GetStorageStatus(nc *nats.Conn) (*StorageStatusOutput, error) {
	msg, err := nc.Request("spinifex.storage.config", []byte("{}"), 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("storage config request: %w", err)
	}

	var cfg types.StorageConfigResponse
	if err := json.Unmarshal(msg.Data, &cfg); err != nil {
		return nil, fmt.Errorf("parse storage config: %w", err)
	}

	dbStatuses := make([]DBNodeStatus, len(cfg.DBNodes))
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	for i, db := range cfg.DBNodes {
		dbStatuses[i] = DBNodeStatus{
			ID:   db.ID,
			Host: db.Host,
			Port: db.Port,
		}
		wg.Add(1)
		go func(idx int, host string, port int) {
			defer wg.Done()
			queryDBNodeStatus(ctx, &dbStatuses[idx], host, port)
		}(i, db.Host, db.Port)
	}
	wg.Wait()

	return &StorageStatusOutput{
		Encoding: StorageEncodingOutput{
			Type:         "Reed-Solomon",
			DataShards:   cfg.Encoding.DataShards,
			ParityShards: cfg.Encoding.ParityShards,
		},
		DBNodes:    dbStatuses,
		ShardNodes: cfg.ShardNodes,
		Buckets:    cfg.Buckets,
	}, nil
}

// predastoreStatusResponse matches the predastore s3db.StatusResponse JSON shape.
type predastoreStatusResponse struct {
	NodeID     string `json:"node_id"`
	State      string `json:"state"`
	Leader     string `json:"leader"`
	LeaderAddr string `json:"leader_addr"`
	Term       string `json:"term"`
	CommitIdx  string `json:"commit_index"`
	AppliedIdx string `json:"applied_index"`
	IsLeader   bool   `json:"is_leader"`
}

func queryDBNodeStatus(ctx context.Context, out *DBNodeStatus, host string, port int) {
	// Resolve 0.0.0.0 to a routable address for HTTPS.
	queryHost := host
	if queryHost == "0.0.0.0" {
		queryHost = "127.0.0.1"
	}

	healthURL := "https://" + net.JoinHostPort(queryHost, strconv.Itoa(port)) + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return
	}
	resp, err := storageHTTPClient.Do(req)
	if err != nil {
		slog.Debug("queryDBNodeStatus: health check failed", "host", host, "port", port, "err", err)
		return
	}
	resp.Body.Close()
	out.Healthy = resp.StatusCode == http.StatusOK

	statusURL := "https://" + net.JoinHostPort(queryHost, strconv.Itoa(port)) + "/status"
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return
	}
	resp, err = storageHTTPClient.Do(req)
	if err != nil {
		slog.Debug("queryDBNodeStatus: status check failed", "host", host, "port", port, "err", err)
		return
	}
	defer resp.Body.Close()

	var status predastoreStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		slog.Debug("queryDBNodeStatus: failed to decode status", "host", host, "port", port, "err", err)
		return
	}

	out.State = status.State
	out.Leader = status.Leader
	out.LeaderAddr = status.LeaderAddr
	out.Term = status.Term
	out.CommitIdx = status.CommitIdx
	out.AppliedIdx = status.AppliedIdx
	out.IsLeader = status.IsLeader
}
