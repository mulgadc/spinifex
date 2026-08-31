package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/nats-io/nats.go/jetstream"
)

// ipsecStatusPrefix is the key prefix for a node's IPsec state record in the
// cluster-state bucket.
const ipsecStatusPrefix = "ipsecstatus."

// ipsecStatusFreshness bounds how long a status record counts for. A node that
// has stopped publishing is not evidence that it is still configured. Must stay
// a large multiple of the interval that refreshes these records.
const ipsecStatusFreshness = 5 * time.Minute

// ipsecLivenessWindow bounds how long a node counts as live on its heartbeat
// alone. Heartbeats run at heartbeatInterval, so this is six of them: long
// enough to ride out a slow pass, short enough that a node taken out of service
// stops holding the cluster's encryption decision hostage.
const ipsecLivenessWindow = 60 * time.Second

// ipsecStatusRecord is what a node publishes about its own IPsec state. The
// timestamp is the point of the record: state that cannot go stale cannot
// distinguish a configured peer from one that stopped answering.
type ipsecStatusRecord struct {
	Node        string    `json:"node"`
	Ready       bool      `json:"ready"`
	NBReachable bool      `json:"nb_reachable"`
	Written     time.Time `json:"written"`
}

// KVIPSecBarrier carries per-node IPsec state over the cluster-state KV.
type KVIPSecBarrier struct {
	kv jetstream.KeyValue
	// now is a var for tests; staleness is otherwise untestable without sleeping.
	now func() time.Time
}

var _ host.IPSecBarrier = (*KVIPSecBarrier)(nil)

// NewKVIPSecBarrier returns a barrier over kv, or nil if kv is nil. A nil
// barrier is meaningful to the caller: it falls back to local knowledge, which
// is the right answer for a cluster with no peers to be out of step with.
func NewKVIPSecBarrier(kv jetstream.KeyValue) *KVIPSecBarrier {
	if kv == nil {
		return nil
	}
	return &KVIPSecBarrier{kv: kv, now: time.Now}
}

// ipsecBarrier returns the cluster readiness barrier, or a nil interface when
// there is no cluster KV to carry it. Returning the concrete nil pointer would
// give host a non-nil interface holding nil, which is not the same thing.
func (d *Daemon) ipsecBarrier() host.IPSecBarrier {
	if d.jsManager == nil || d.jsManager.clusterKV == nil {
		return nil
	}
	return NewKVIPSecBarrier(d.jsManager.clusterKV)
}

// Publish records this node's own IPsec state.
func (b *KVIPSecBarrier) Publish(ctx context.Context, node string, status host.IPSecNodeStatus) error {
	if node == "" {
		return errors.New("node name unset")
	}
	data, err := json.Marshal(ipsecStatusRecord{
		Node:        node,
		Ready:       status.Ready,
		NBReachable: status.NBReachable,
		Written:     b.now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("marshal IPsec status: %w", err)
	}
	if _, err := b.kv.Put(ctx, ipsecStatusPrefix+node, data); err != nil {
		return fmt.Errorf("put IPsec status for %s: %w", node, err)
	}
	return nil
}

// Cluster returns the last published status of every live node. A node with a
// fresh record is live by definition; one without is live only if it is still
// heartbeating, and otherwise drops out of the set entirely.
//
// The two-step matters. A node whose KV writes are failing stops heartbeating
// too, so it leaves the set rather than being read as a chassis that lost its
// configuration — which would strip encryption from the peers still talking.
func (b *KVIPSecBarrier) Cluster(ctx context.Context, nodes []string) (map[string]host.IPSecNodeStatus, error) {
	cluster := make(map[string]host.IPSecNodeStatus, len(nodes))
	for _, node := range nodes {
		rec, err := b.readStatus(ctx, node)
		if err != nil {
			return nil, err
		}
		if rec != nil && b.now().UTC().Sub(rec.Written) <= ipsecStatusFreshness {
			cluster[node] = host.IPSecNodeStatus{Ready: rec.Ready, NBReachable: rec.NBReachable}
			continue
		}
		alive, err := b.heartbeating(ctx, node)
		if err != nil {
			return nil, err
		}
		if alive {
			cluster[node] = host.IPSecNodeStatus{}
		}
	}
	return cluster, nil
}

// readStatus returns nil for a node that has published nothing, and for a record
// that cannot be parsed — both mean "no usable claim of readiness".
func (b *KVIPSecBarrier) readStatus(ctx context.Context, node string) (*ipsecStatusRecord, error) {
	entry, err := b.kv.Get(ctx, ipsecStatusPrefix+node)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get IPsec status for %s: %w", node, err)
	}
	rec, ok := decodeStatus(entry.Value())
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

// decodeStatus reports usability rather than an error: a record that cannot be
// parsed is not a claim of readiness, which is a state this cares about and not
// a failure to read the KV.
func decodeStatus(data []byte) (ipsecStatusRecord, bool) {
	var rec ipsecStatusRecord
	if json.Unmarshal(data, &rec) != nil {
		return ipsecStatusRecord{}, false
	}
	return rec, true
}

// heartbeating reports whether the node has written a heartbeat recently enough
// to still count as part of the cluster.
func (b *KVIPSecBarrier) heartbeating(ctx context.Context, node string) (bool, error) {
	entry, err := b.kv.Get(ctx, "heartbeat."+node)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get heartbeat for %s: %w", node, err)
	}
	written, ok := decodeHeartbeatTime(entry.Value())
	if !ok {
		return false, nil
	}
	return b.now().UTC().Sub(written) <= ipsecLivenessWindow, nil
}

// decodeHeartbeatTime returns when the heartbeat was written. A beat that cannot
// be parsed or dated vouches for nothing, which is an answer rather than a fault.
func decodeHeartbeatTime(data []byte) (time.Time, bool) {
	var h Heartbeat
	if json.Unmarshal(data, &h) != nil {
		return time.Time{}, false
	}
	written, err := time.Parse(time.RFC3339, h.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return written.UTC(), true
}
