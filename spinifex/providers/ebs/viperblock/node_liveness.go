package viperblock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// nodeLivenessWindow is how recently a node must have been seen for its dirty
// marker to bar a takeover. It is six heartbeats and above volumeLeaseTTL, and is
// compared across node clocks, where NTP skew is small against 60s.
const nodeLivenessWindow = 60 * time.Second

// nodeLiveness reports when a node was last seen alive. found is false for a
// node that has never been seen or whose record has expired; an error means the
// answer is unknown, which a caller deciding on a takeover must not read as dead.
type nodeLiveness interface {
	LastSeen(ctx context.Context, node string) (seen time.Time, found bool, err error)
}

// heartbeatLiveness reads node heartbeats from the cluster-state bucket the
// daemon writes. It opens the bucket on first use and never creates it: the
// daemon owns it, and a viperblockd that starts first must not claim it with the
// wrong settings.
type heartbeatLiveness struct {
	nc *nats.Conn

	mu sync.Mutex
	kv jetstream.KeyValue
}

var _ nodeLiveness = (*heartbeatLiveness)(nil)

func newHeartbeatLiveness(nc *nats.Conn) *heartbeatLiveness {
	return &heartbeatLiveness{nc: nc}
}

func (l *heartbeatLiveness) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.kv != nil {
		return l.kv, nil
	}
	js, err := jetstream.New(l.nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := js.KeyValue(ctx, clusterv1.ClusterStateBucket)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", clusterv1.ClusterStateBucket, err)
	}
	l.kv = kv
	return kv, nil
}

// LastSeen returns when node last wrote a heartbeat. No record, or one that
// cannot be dated, is found=false: the daemon's bucket expires a node's entry
// once it has been silent long enough.
func (l *heartbeatLiveness) LastSeen(ctx context.Context, node string) (time.Time, bool, error) {
	kv, err := l.bucket(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	entry, err := kv.Get(ctx, clusterv1.HeartbeatKey(node))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read heartbeat for %s: %w", node, err)
	}
	written, ok := heartbeatTime(entry.Value())
	return written, ok, nil
}

// heartbeatTime dates a stored heartbeat. One that cannot be read vouches for
// nothing, which is an answer rather than a fault.
func heartbeatTime(data []byte) (time.Time, bool) {
	var beat clusterv1.HeartbeatTimestamp
	if json.Unmarshal(data, &beat) != nil {
		return time.Time{}, false
	}
	return beat.Time()
}
