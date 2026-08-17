package gateway_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

// respondAsNodes answers the discovery subject as n distinct nodes and reports
// how many discovery rounds were served.
func respondAsNodes(t *testing.T, nc *nats.Conn, n int) *atomic.Int64 {
	t.Helper()

	var rounds atomic.Int64
	for i := range n {
		node := string(rune('a' + i))
		sub, err := nc.Subscribe("spinifex.nodes.discover", func(msg *nats.Msg) {
			if node == "a" {
				rounds.Add(1)
			}
			payload, err := json.Marshal(types.NodeDiscoverResponse{Node: node})
			if err != nil {
				return
			}
			_ = msg.Respond(payload)
		})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return &rounds
}

// Discovery fronts eight EC2 handlers, so whatever it costs is added to every
// one of their calls. The gather counts whoever answers and therefore cannot
// know when to stop, so it always sits out its whole 500ms timeout — that cost
// has to be paid once in a while, not once per request.
func TestDiscoverActiveNodesKeepsItsCostOffTheRequestPath(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	respondAsNodes(t, nc, 4)

	gw := &gateway.GatewayConfig{NATSConn: nc, ExpectedNodes: 4}

	if count := gw.DiscoverActiveNodes(context.Background()); count != 4 {
		t.Fatalf("DiscoverActiveNodes = %d, want 4", count)
	}

	start := time.Now()
	for range 5 {
		gw.DiscoverActiveNodes(context.Background())
	}
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("five further discoveries took %v; each was paying the fan-out timeout again", elapsed)
	}
}

// Membership does not change per request. Re-deriving it on every call put a
// fan-out in front of every API call that needs it, which is most of them.
func TestDiscoverActiveNodesReusesARecentAnswer(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	rounds := respondAsNodes(t, nc, 3)

	gw := &gateway.GatewayConfig{NATSConn: nc, ExpectedNodes: 3}

	if count := gw.DiscoverActiveNodes(context.Background()); count != 3 {
		t.Fatalf("first discovery = %d, want 3", count)
	}
	first := rounds.Load()

	for range 5 {
		if count := gw.DiscoverActiveNodes(context.Background()); count != 3 {
			t.Fatalf("cached discovery = %d, want 3", count)
		}
	}

	if got := rounds.Load(); got != first {
		t.Errorf("discovery ran %d more time(s) within the TTL, want the cached answer reused", got-first)
	}
}

// A fallback is not evidence of anything: caching one would make a momentary
// NATS problem outlive itself for the whole TTL.
func TestDiscoverActiveNodesDoesNotCacheAFallback(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	gw := &gateway.GatewayConfig{NATSConn: nc, ExpectedNodes: 2}

	if count := gw.DiscoverActiveNodes(context.Background()); count != 2 {
		t.Fatalf("fallback = %d, want the configured node count", count)
	}

	rounds := respondAsNodes(t, nc, 2)
	if count := gw.DiscoverActiveNodes(context.Background()); count != 2 {
		t.Fatalf("second discovery = %d, want 2", count)
	}
	if rounds.Load() == 0 {
		t.Error("no discovery was published after a fallback, so the fallback had been cached")
	}
}
