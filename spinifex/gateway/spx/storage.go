package spx

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pds "github.com/mulgadc/predastore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

// MetaNodeStatus is a single meta node's live status merged with its config.
type MetaNodeStatus struct {
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
	Encoding  StorageEncodingOutput   `json:"encoding"`
	MetaNodes []MetaNodeStatus        `json:"meta_nodes"`
	BlobNodes []types.StorageBlobNode `json:"blob_nodes"`
	Buckets   []types.StorageBucket   `json:"buckets"`
}

// StorageEncodingOutput adds the type label to the encoding config.
type StorageEncodingOutput struct {
	Type         string `json:"type"`
	DataShards   int    `json:"data_shards"`
	ParityShards int    `json:"parity_shards"`
}

// metaNodeQueryTimeout bounds the whole parallel round of meta node dials, so
// one unreachable node cannot stall the CLI command waiting on the others.
const metaNodeQueryTimeout = 2 * time.Second

// GetStorageStatus fetches predastore topology via NATS, then dials each meta
// node's raft status directly and in parallel. rootCAs is the cluster CA
// pool: every meta node in the cluster is queried, not just a local one, so
// verifying each one's identity against the shared CA (rather than a local
// TLS config only the node owning it could read) is what makes every dial
// trusted.
func GetStorageStatus(nc *nats.Conn, rootCAs *x509.CertPool) (*StorageStatusOutput, error) {
	msg, err := nc.Request("spinifex.storage.config", []byte("{}"), 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("storage config request: %w", err)
	}

	var cfg types.StorageConfigResponse
	if err := json.Unmarshal(msg.Data, &cfg); err != nil {
		return nil, fmt.Errorf("parse storage config: %w", err)
	}

	metaStatuses := make([]MetaNodeStatus, len(cfg.MetaNodes))
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), metaNodeQueryTimeout)
	defer cancel()

	for i, meta := range cfg.MetaNodes {
		metaStatuses[i] = MetaNodeStatus{
			ID:   meta.ID,
			Host: meta.Host,
			Port: meta.Port,
		}
		wg.Add(1)
		go func(idx int, host string, port int) {
			defer wg.Done()
			queryMetaNodeStatus(ctx, &metaStatuses[idx], rootCAs, host, port)
		}(i, meta.Host, meta.Port)
	}
	wg.Wait()

	return &StorageStatusOutput{
		Encoding: StorageEncodingOutput{
			Type:         "Reed-Solomon",
			DataShards:   cfg.Encoding.DataShards,
			ParityShards: cfg.Encoding.ParityShards,
		},
		MetaNodes: metaStatuses,
		BlobNodes: cfg.BlobNodes,
		Buckets:   cfg.Buckets,
	}, nil
}

// queryMetaNodeStatus dials one meta node's raft status directly — the
// opcode-multiplexed RPC predastore's meta nodes actually speak, not the
// HTTPS endpoints they never served — and fills out with what it reports. A
// node that does not answer within ctx leaves out untouched (Healthy stays
// false and every raft field stays blank), matching the CLI's existing
// contract for an unhealthy node.
func queryMetaNodeStatus(ctx context.Context, out *MetaNodeStatus, rootCAs *x509.CertPool, host string, port int) {
	nodeID, ok := metaNodeID(out.ID)
	if !ok {
		slog.Debug("queryMetaNodeStatus: invalid node id", "id", out.ID, "host", host, "port", port)
		return
	}

	// Resolve 0.0.0.0 to a routable address.
	queryHost := host
	if queryHost == "0.0.0.0" {
		queryHost = "127.0.0.1"
	}

	// A single-host, single-node config is all NodeStatus needs to resolve
	// and dial this one node: it queries that replica only, never redirects,
	// and asks for no local TLS identity of its own.
	cfg := &pds.Config{
		Hosts: []pds.HostConfig{
			{
				ID:    pds.HostID(nodeID),
				Addr:  queryHost,
				Nodes: []pds.NodeConfig{{ID: nodeID, Role: pds.RoleMeta, Port: port}},
			},
		},
	}

	status, err := pds.NodeStatus(ctx, cfg, nodeID, rootCAs)
	if err != nil {
		slog.Debug("queryMetaNodeStatus: status probe failed", "id", out.ID, "host", host, "port", port, "err", err)
		return
	}
	applyMetaStatus(out, status)
}

// metaNodeID converts a meta node's id — plain int on the wire, predastore's
// NodeID (uint64) internally — validating it is never negative first, since
// predastore ids never are and a blind conversion could wrap.
func metaNodeID(id int) (pds.NodeID, bool) {
	if id < 0 {
		return 0, false
	}
	return pds.NodeID(id), true //#nosec G115 -- id validated non-negative above
}

// applyMetaStatus maps a predastore raft Status into a MetaNodeStatus.
// Reaching here means the node answered, so Healthy is unconditionally true.
func applyMetaStatus(out *MetaNodeStatus, status pds.Status) {
	out.Healthy = true
	out.State = status.State
	out.Leader = status.Leader
	out.LeaderAddr = status.LeaderAddr
	out.Term = status.Term
	out.CommitIdx = status.CommitIndex
	out.AppliedIdx = status.AppliedIndex
	out.IsLeader = status.IsLeader
}
