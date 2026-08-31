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

// IPSecReadyPrefix is the key prefix for a node's IPsec readiness record in the
// cluster-state bucket.
const IPSecReadyPrefix = "ipsecready."

// ipsecReadyFreshness bounds how long a readiness record counts for. A node
// that has stopped publishing is not evidence that it is still configured, and
// treating it as such is what lets encryption stay asserted over a chassis that
// came back up unconfigured. Must stay comfortably above the reconcile interval
// that refreshes these records.
var ipsecReadyFreshness = 5 * time.Minute

// ipsecReadyRecord is what a node publishes about its own local IPsec setup.
// The timestamp is the point of the record: readiness that cannot go stale
// cannot distinguish a configured peer from an absent one.
type ipsecReadyRecord struct {
	Node    string    `json:"node"`
	Ready   bool      `json:"ready"`
	Written time.Time `json:"written"`
}

// KVIPSecBarrier carries per-node IPsec readiness over the cluster-state KV.
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

// PublishLocalReady records this node's own IPsec completion.
func (b *KVIPSecBarrier) PublishLocalReady(ctx context.Context, node string, ready bool) error {
	if node == "" {
		return errors.New("node name unset")
	}
	data, err := json.Marshal(ipsecReadyRecord{Node: node, Ready: ready, Written: b.now().UTC()})
	if err != nil {
		return fmt.Errorf("marshal IPsec readiness: %w", err)
	}
	if _, err := b.kv.Put(ctx, IPSecReadyPrefix+node, data); err != nil {
		return fmt.Errorf("put IPsec readiness for %s: %w", node, err)
	}
	return nil
}

// NodesReady reports whether every named node has a fresh ready record, and
// names those that do not. A node that is absent, stale, unready or unparseable
// is pending: only a positive, current claim of readiness counts.
func (b *KVIPSecBarrier) NodesReady(ctx context.Context, nodes []string) (bool, []string, error) {
	var pending []string
	for _, node := range nodes {
		entry, err := b.kv.Get(ctx, IPSecReadyPrefix+node)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			pending = append(pending, node)
			continue
		}
		if err != nil {
			return false, nil, fmt.Errorf("get IPsec readiness for %s: %w", node, err)
		}
		var rec ipsecReadyRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			pending = append(pending, node)
			continue
		}
		if !rec.Ready || b.now().UTC().Sub(rec.Written) > ipsecReadyFreshness {
			pending = append(pending, node)
		}
	}
	return len(pending) == 0, pending, nil
}
