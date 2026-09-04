package instancecache

import (
	"context"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// HeartbeatInterval mirrors the cadence the daemon publishes heartbeats
	// at. Exported so the daemon can assert the two have not drifted; a
	// longer publish interval would make NodeStaleAfter flap.
	HeartbeatInterval = 10 * time.Second

	// NodeStaleAfter is three and a half missed beats. Two flaps every
	// instance on a node over a GC pause or a NATS reconnect, four is slow to
	// tell the truth.
	NodeStaleAfter = 7 * HeartbeatInterval / 2

	// heartbeatPrefix is the cluster-state key prefix the daemon writes each
	// node's heartbeat under.
	heartbeatPrefix = "heartbeat."

	// livenessMemo collapses a burst of requests into one KV read. Kept far
	// below NodeStaleAfter so it cannot change a verdict: the worst it does is
	// report a node stale a second late.
	livenessMemo = time.Second
)

// NodeState is what the heartbeat store says about a node. Unknown is a
// distinct answer from Stale: an unreadable store is not evidence that a node
// is gone, and the two must not collapse into one.
type NodeState int

const (
	NodeUnknown NodeState = iota
	NodeLive
	NodeStale
)

// heartbeat is the subset of the daemon's heartbeat record this needs.
// Decoding only these two fields keeps the gateway independent of the capacity
// figures alongside them, which change for unrelated reasons.
type heartbeat struct {
	Node      string `json:"node"`
	Timestamp string `json:"timestamp"`
}

// Liveness answers whether a node is still heartbeating. It is a read-only
// view over the cluster-state bucket and holds no watch of its own.
type Liveness struct {
	store *kvstore.Store[heartbeat]
	now   func() time.Time

	mu        sync.Mutex
	seen      map[string]time.Time
	fetchedAt time.Time
	readOK    bool
}

// NewLiveness opens a reader over the cluster-state bucket. The caller
// resolves the bucket name, as it does for the record cache, so this package
// never has to know the daemon's key-space constants.
func NewLiveness(js jetstream.JetStream, bucket kvstore.Config) *Liveness {
	return &Liveness{
		store: kvstore.New[heartbeat](js, bucket),
		now:   time.Now,
		seen:  make(map[string]time.Time),
	}
}

// State reports whether node is live, stale, or unknown. A node with no
// heartbeat at all is Unknown rather than Stale: it may simply never have
// published one, which is not proof that it died.
func (l *Liveness) State(ctx context.Context, node string) NodeState {
	if l == nil || node == "" {
		return NodeUnknown
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked(ctx)

	if !l.readOK {
		return NodeUnknown
	}
	written, ok := l.seen[node]
	if !ok {
		return NodeUnknown
	}
	if l.now().UTC().Sub(written) > NodeStaleAfter {
		return NodeStale
	}
	return NodeLive
}

// refreshLocked re-reads every heartbeat when the memo has expired. A failed
// read clears readOK rather than keeping the previous answers, so a store the
// gateway cannot reach degrades to Unknown instead of asserting liveness it
// can no longer confirm.
func (l *Liveness) refreshLocked(ctx context.Context) {
	if l.now().Sub(l.fetchedAt) < livenessMemo {
		return
	}
	l.fetchedAt = l.now()

	beats, err := l.store.List(ctx, heartbeatPrefix)
	if err != nil {
		l.readOK = false
		return
	}

	seen := make(map[string]time.Time, len(beats))
	for _, b := range beats {
		written, err := time.Parse(time.RFC3339, b.Timestamp)
		if err != nil || b.Node == "" {
			continue
		}
		seen[b.Node] = written.UTC()
	}
	l.seen = seen
	l.readOK = true
}
