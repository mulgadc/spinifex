package instancecache

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// newTestLiveness opens a cluster-state-shaped bucket and returns a Liveness
// over it, plus the raw KV handle so a test can write heartbeats directly.
func newTestLiveness(t *testing.T) (*Liveness, jetstream.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	bucket := fmt.Sprintf("cluster-%d", time.Now().UnixNano())
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)

	return NewLiveness(js, kvstore.Config{Name: bucket, History: 1, Replicas: 1}), kv
}

// putHeartbeat writes a heartbeat for node stamped age before the test's now.
func putHeartbeat(t *testing.T, kv jetstream.KeyValue, node string, now time.Time, age time.Duration) {
	t.Helper()
	data, err := json.Marshal(heartbeat{
		Node:      node,
		Timestamp: now.Add(-age).UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), heartbeatPrefix+node, data)
	require.NoError(t, err)
}

// A node beating inside the threshold is live; one past it is stale. The
// boundary is checked from both sides so the comparison cannot be inverted
// without a failure.
func TestLiveness_StaleOnlyPastThreshold(t *testing.T) {
	l, kv := newTestLiveness(t)
	now := time.Now()
	l.now = func() time.Time { return now }

	putHeartbeat(t, kv, "fresh", now, time.Second)
	putHeartbeat(t, kv, "justInside", now, NodeStaleAfter-time.Second)
	putHeartbeat(t, kv, "justOutside", now, NodeStaleAfter+time.Second)

	ctx := context.Background()
	require.Equal(t, NodeLive, l.State(ctx, "fresh"))

	l.fetchedAt = time.Time{}
	require.Equal(t, NodeLive, l.State(ctx, "justInside"))
	require.Equal(t, NodeStale, l.State(ctx, "justOutside"))
}

// A node that has never published a heartbeat is Unknown, not Stale. Silence
// from a node nobody has heard from is not evidence that it died.
func TestLiveness_UnheardNodeIsUnknownNotStale(t *testing.T) {
	l, kv := newTestLiveness(t)
	now := time.Now()
	l.now = func() time.Time { return now }
	putHeartbeat(t, kv, "known", now, time.Second)

	require.Equal(t, NodeUnknown, l.State(context.Background(), "never-seen"))
}

// An unreadable store degrades every answer to Unknown rather than keeping the
// last one. Asserting liveness the gateway can no longer confirm is the lie
// this whole path exists to remove.
func TestLiveness_UnreadableStoreIsUnknown(t *testing.T) {
	l, kv := newTestLiveness(t)
	now := time.Now()
	l.now = func() time.Time { return now }
	putHeartbeat(t, kv, "node-a", now, time.Second)

	ctx := context.Background()
	require.Equal(t, NodeLive, l.State(ctx, "node-a"))

	// Cancelled context stands in for a bucket the gateway cannot reach.
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	l.fetchedAt = time.Time{}
	require.Equal(t, NodeUnknown, l.State(dead, "node-a"))
}

// An empty node name is Unknown, and a nil Liveness answers rather than
// panicking, so a caller wired without one degrades instead of crashing.
func TestLiveness_EmptyNodeAndNilReceiver(t *testing.T) {
	l, _ := newTestLiveness(t)
	require.Equal(t, NodeUnknown, l.State(context.Background(), ""))

	var nilLiveness *Liveness
	require.Equal(t, NodeUnknown, nilLiveness.State(context.Background(), "node-a"))
}

// The memo collapses repeated reads but must not outlive its window, or a
// node that goes silent would keep reporting live.
func TestLiveness_MemoExpires(t *testing.T) {
	l, kv := newTestLiveness(t)
	now := time.Now()
	l.now = func() time.Time { return now }
	putHeartbeat(t, kv, "node-a", now, time.Second)

	ctx := context.Background()
	require.Equal(t, NodeLive, l.State(ctx, "node-a"))

	// Same heartbeat, but the clock has moved past the threshold. Within the
	// memo the cached answer stands; past it the re-read reports stale.
	now = now.Add(NodeStaleAfter + time.Second)
	l.fetchedAt = now.Add(-livenessMemo / 2)
	require.Equal(t, NodeStale, l.State(ctx, "node-a"),
		"staleness is judged against the current clock, not the read time")
}
