package instancecache

import (
	"encoding/json"
	"time"
)

// LivenessMemo is the read-collapsing window, exported so a test can position
// itself either side of it.
const LivenessMemo = livenessMemo

// HeartbeatRecord returns the key and payload the daemon writes for a node's
// heartbeat, so a test can seed the bucket without importing the daemon.
func HeartbeatRecord(node string, stamped time.Time) (string, []byte, error) {
	data, err := json.Marshal(heartbeat{
		Node:      node,
		Timestamp: stamped.UTC().Format(time.RFC3339),
	})
	return heartbeatPrefix + node, data, err
}

// SetClock replaces the clock staleness is judged against.
func (l *Liveness) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// SetFetchedAt moves the memo's last-read time; the zero value expires it.
func (l *Liveness) SetFetchedAt(t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fetchedAt = t
}
