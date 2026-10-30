package viperblock

import (
	"context"
	"time"
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
