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
// subject. Run refreshes the count in the background, so a request that needs
// it never waits for the discovery fan-out.
type NodeDiscovery struct {
	NATS     *nats.Conn
	Expected int

	mu    sync.RWMutex
	count int
}

// activeNodesTTL is the interval between background refreshes. A var only so
// tests can shorten it.
var activeNodesTTL = 5 * time.Second

// discoverActiveNodesTimeout is how long the discovery fan-out collects replies.
// A var only so tests can shorten it.
var discoverActiveNodesTimeout = 500 * time.Millisecond

// ActiveNodes returns the count from the last good refresh, or Expected when
// there is none. It never gathers, so it adds no latency to a request.
func (d *NodeDiscovery) ActiveNodes(context.Context) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.count == 0 {
		return d.Expected
	}
	return d.count
}

// DiscoverNow gathers on the caller's path, for a check that must not use a
// background count. It returns Expected when no node answers.
func (d *NodeDiscovery) DiscoverNow(ctx context.Context) int {
	if count, ok := d.discover(ctx); ok {
		return count
	}
	return d.Expected
}

// Run refreshes the count at once and then every activeNodesTTL until ctx is
// done. A failed refresh clears the count, so ActiveNodes falls back to Expected.
func (d *NodeDiscovery) Run(ctx context.Context) {
	ticker := time.NewTicker(activeNodesTTL)
	defer ticker.Stop()
	for {
		count, ok := d.discover(ctx)
		if !ok {
			count = 0
		}
		d.mu.Lock()
		d.count = count
		d.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// discover counts every node that answers, which can exceed the configured
// count during a grow. Stopping at that count would drop a live node from the
// fan-outs that follow. It returns false on an error or when no node answers.
func (d *NodeDiscovery) discover(ctx context.Context) (int, bool) {
	if d.NATS == nil {
		slog.WarnContext(ctx, "DiscoverActiveNodes: NATS connection not available, using ExpectedNodes fallback", "fallback", d.Expected)
		return 0, false
	}

	frames, _, err := natsmsg.Gather(ctx, d.NATS, clusterv1.NodesDiscoverSubject, []byte("{}"),
		natsmsg.GatherOpts{Timeout: discoverActiveNodesTimeout})
	if err != nil {
		slog.ErrorContext(ctx, "DiscoverActiveNodes: fan-out failed, using ExpectedNodes fallback", "err", err, "fallback", d.Expected)
		return 0, false
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
		return 0, false
	}

	slog.DebugContext(ctx, "DiscoverActiveNodes: Discovered active nodes", "count", activeNodes)
	return activeNodes, true
}
