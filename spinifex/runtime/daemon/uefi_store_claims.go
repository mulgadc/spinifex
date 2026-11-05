package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// clusterHeartbeats satisfies vm.NodeLiveness from the heartbeats every daemon
// writes to the cluster-state bucket.
type clusterHeartbeats struct {
	kv jetstream.KeyValue
}

var _ vm.NodeLiveness = clusterHeartbeats{}

// LastSeen returns when node last wrote a heartbeat. No entry is found=false;
// a failed read, or an entry that cannot be dated, is an error, because a beat
// that exists may be a live node's.
func (h clusterHeartbeats) LastSeen(ctx context.Context, node string) (time.Time, bool, error) {
	entry, err := h.kv.Get(ctx, clusterv1.HeartbeatKey(node))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read heartbeat for %s: %w", node, err)
	}
	written, ok := clusterv1.HeartbeatWritten(entry.Value())
	if !ok {
		return time.Time{}, false, fmt.Errorf("heartbeat for %s cannot be dated", node)
	}
	return written, true, nil
}

// heartbeatLiveness returns nil, not a typed nil, before the cluster-state
// bucket is open, so a launch sees liveness as unwired rather than panicking.
func (d *Daemon) heartbeatLiveness() vm.NodeLiveness {
	if d.jsManager == nil || d.jsManager.clusterKV == nil {
		return nil
	}
	return clusterHeartbeats{kv: d.jsManager.clusterKV}
}

// uefiStoreRecords returns nil, not a typed nil, when the store bucket is not
// open, so a launch sees store records as unwired.
func (d *Daemon) uefiStoreRecords() vm.UEFIStoreRecords {
	if d.jsManager == nil || d.jsManager.UEFIStores() == nil {
		return nil
	}
	return d.jsManager.UEFIStores()
}
