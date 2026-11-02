package awsgw

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// NodeDiscovery counts the daemon nodes that answer on the cluster discovery
// subject. It remembers the last real answer, so the fan-out runs once per
// activeNodesTTL rather than once per request that needs the node count.
type NodeDiscovery struct {
	NATS     *nats.Conn
	Expected int

	mu    sync.RWMutex
	count int
	at    time.Time
}

const activeNodesTTL = 5 * time.Second

// discoverActiveNodesTimeout is how long the discovery fan-out collects replies.
// A var only so tests can shorten it.
var discoverActiveNodesTimeout = 500 * time.Millisecond

// ActiveNodes discovers the number of active spinifex daemon nodes in the
// cluster by publishing a discovery request and counting unique responses. It
// carries the request context so the discovery fan-out joins the caller's trace.
// Returns the number of active nodes (minimum 1 if fallback is needed).
//
// The result is cached for activeNodesTTL. The gather itself stays unbounded:
// it counts whoever answers, which may legitimately exceed the configured node
// count during a grow, and bounding it by that count would stop early and drop
// a live node's resources from every fan-out that follows.
func (d *NodeDiscovery) ActiveNodes(ctx context.Context) int {
	if d.NATS == nil {
		slog.WarnContext(ctx, "DiscoverActiveNodes: NATS connection not available, using ExpectedNodes fallback", "fallback", d.Expected)
		return d.Expected
	}

	if count, ok := d.cachedActiveNodes(); ok {
		return count
	}

	frames, _, err := natsmsg.Gather(ctx, d.NATS, clusterv1.NodesDiscoverSubject, []byte("{}"),
		natsmsg.GatherOpts{Timeout: discoverActiveNodesTimeout})
	if err != nil {
		slog.ErrorContext(ctx, "DiscoverActiveNodes: fan-out failed, using ExpectedNodes fallback", "err", err, "fallback", d.Expected)
		return d.Expected
	}

	nodesSeen := make(map[string]bool)
	for _, frame := range frames {
		var response clusterv1.NodeDiscoverResponse
		if err := json.Unmarshal(frame.Data, &response); err != nil {
			slog.DebugContext(ctx, "DiscoverActiveNodes: Failed to unmarshal response", "err", err)
			continue
		}
		nodesSeen[response.Node] = true
	}

	activeNodes := len(nodesSeen)
	if activeNodes == 0 {
		slog.WarnContext(ctx, "DiscoverActiveNodes: No nodes responded, using ExpectedNodes fallback", "fallback", d.Expected)
		return d.Expected
	}

	d.rememberActiveNodes(activeNodes)
	slog.DebugContext(ctx, "DiscoverActiveNodes: Discovered active nodes", "count", activeNodes)
	return activeNodes
}

// cachedActiveNodes returns a count discovered within the TTL. Only a real
// discovery is cached: a fallback is not evidence of anything and caching one
// would make a momentary NATS problem outlive itself.
func (d *NodeDiscovery) cachedActiveNodes() (int, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.count == 0 || time.Since(d.at) > activeNodesTTL {
		return 0, false
	}
	return d.count, true
}

func (d *NodeDiscovery) rememberActiveNodes(count int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count = count
	d.at = time.Now()
}
