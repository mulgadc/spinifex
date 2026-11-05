package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// clusterHeartbeats satisfies vm.NodeLiveness from the heartbeats every daemon
// writes to the cluster-state bucket.
type clusterHeartbeats struct {
	kv jetstream.KeyValue
	js jetstream.JetStream
}

var _ vm.NodeLiveness = clusterHeartbeats{}

// LastSeen returns when node last wrote a heartbeat. No entry is found=false;
// a failed read, or an entry that cannot be dated, is an error, because a beat
// that exists may be a live node's.
//
// A replica that lags can only answer with an older beat, so a live one is
// returned as read and anything else is read again from the stream leader.
func (h clusterHeartbeats) LastSeen(ctx context.Context, node string) (time.Time, bool, error) {
	key := clusterv1.HeartbeatKey(node)
	if entry, err := h.kv.Get(ctx, key); err == nil {
		if written, ok := clusterv1.HeartbeatWritten(entry.Value()); ok && clusterv1.LiveAt(written, time.Now()) {
			return written, true, nil
		}
	}
	if h.js == nil {
		return time.Time{}, false, fmt.Errorf("read heartbeat for %s: no JetStream client to confirm it with the leader", node)
	}
	bucket := clusterv1.ClusterStateBucket
	msg, err := kvstore.LeaderMsgGet(ctx, h.js, "KV_"+bucket, kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + key})
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read heartbeat for %s from the leader: %w", node, err)
	}
	if op := msg.Header.Get(kvOperationHeader); op == "DEL" || op == "PURGE" {
		return time.Time{}, false, nil
	}
	written, ok := clusterv1.HeartbeatWritten(msg.Data)
	if !ok {
		return time.Time{}, false, fmt.Errorf("heartbeat for %s cannot be dated", node)
	}
	return written, true, nil
}

// kvOperationHeader marks a KV stream message that deletes or purges its key.
const kvOperationHeader = "KV-Operation"

// heartbeatLiveness returns nil, not a typed nil, before the cluster-state
// bucket is open, so a launch sees liveness as unwired rather than panicking.
func (d *Daemon) heartbeatLiveness() vm.NodeLiveness {
	if d.jsManager == nil || d.jsManager.clusterKV == nil {
		return nil
	}
	return clusterHeartbeats{kv: d.jsManager.clusterKV, js: d.jsManager.js}
}

// uefiStoreRecords returns nil, not a typed nil, when the store bucket is not
// open, so a launch sees store records as unwired.
func (d *Daemon) uefiStoreRecords() vm.UEFIStoreRecords {
	if d.jsManager == nil || d.jsManager.UEFIStores() == nil {
		return nil
	}
	return d.jsManager.UEFIStores()
}
