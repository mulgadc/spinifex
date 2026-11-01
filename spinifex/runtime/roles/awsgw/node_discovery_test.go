package awsgw_test

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"
	"github.com/nats-io/nats.go"
)

// respondAsNodes answers the discovery subject as n distinct nodes and reports
// how many discovery rounds were served.
func respondAsNodes(t *testing.T, nc *nats.Conn, n int) *atomic.Int64 {
	t.Helper()

	var rounds atomic.Int64
	for i := range n {
		node := string(rune('a' + i))
		sub, err := nc.Subscribe(clusterv1.NodesDiscoverSubject, func(msg *nats.Msg) {
			if node == "a" {
				rounds.Add(1)
			}
			payload, err := json.Marshal(clusterv1.NodeDiscoverResponse{Node: node})
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
// know when to stop, so it always sits out its whole timeout — that cost
// has to be paid once in a while, not once per request.
func TestDiscoverActiveNodesKeepsItsCostOffTheRequestPath(t *testing.T) {
	const timeout = 50 * time.Millisecond
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, timeout)
	_, nc := testutil.StartTestNATS(t)
	respondAsNodes(t, nc, 4)

	gw := &awsgw.NodeDiscovery{NATS: nc, Expected: 4}

	if count := gw.ActiveNodes(context.Background()); count != 4 {
		t.Fatalf("DiscoverActiveNodes = %d, want 4", count)
	}

	start := time.Now()
	for range 5 {
		gw.ActiveNodes(context.Background())
	}
	elapsed := time.Since(start)

	// Five uncached calls would cost five timeouts, so two bounds the cached path well clear of that.
	if elapsed > 2*timeout {
		t.Errorf("five further discoveries took %v; each was paying the fan-out timeout again", elapsed)
	}
}

// Membership does not change per request. Re-deriving it on every call put a
// fan-out in front of every API call that needs it, which is most of them.
func TestDiscoverActiveNodesReusesARecentAnswer(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	_, nc := testutil.StartTestNATS(t)
	rounds := respondAsNodes(t, nc, 3)

	gw := &awsgw.NodeDiscovery{NATS: nc, Expected: 3}

	if count := gw.ActiveNodes(context.Background()); count != 3 {
		t.Fatalf("first discovery = %d, want 3", count)
	}
	first := rounds.Load()

	for range 5 {
		if count := gw.ActiveNodes(context.Background()); count != 3 {
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
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	_, nc := testutil.StartTestNATS(t)

	gw := &awsgw.NodeDiscovery{NATS: nc, Expected: 2}

	if count := gw.ActiveNodes(context.Background()); count != 2 {
		t.Fatalf("fallback = %d, want the configured node count", count)
	}

	rounds := respondAsNodes(t, nc, 2)
	if count := gw.ActiveNodes(context.Background()); count != 2 {
		t.Fatalf("second discovery = %d, want 2", count)
	}
	if rounds.Load() == 0 {
		t.Error("no discovery was published after a fallback, so the fallback had been cached")
	}
}

func startDiscoveryNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	return nc
}

func TestDiscoverActiveNodes_NilNATS(t *testing.T) {
	gw := &awsgw.NodeDiscovery{
		Expected: 3,
		NATS:     nil,
	}

	result := gw.ActiveNodes(context.Background())
	assert.Equal(t, 3, result)
}

func TestDiscoverActiveNodes_NoResponders(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	nc := startDiscoveryNATS(t)

	gw := &awsgw.NodeDiscovery{
		Expected: 5,
		NATS:     nc,
	}

	result := gw.ActiveNodes(context.Background())
	assert.Equal(t, 5, result)
}

func TestDiscoverActiveNodes_WithResponders(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	nc := startDiscoveryNATS(t)

	for _, nodeName := range []string{"node-1", "node-2"} {
		name := nodeName
		_, err := nc.Subscribe(clusterv1.NodesDiscoverSubject, func(msg *nats.Msg) {
			resp := clusterv1.NodeDiscoverResponse{Node: name}
			data, _ := json.Marshal(resp)
			msg.Respond(data)
		})
		require.NoError(t, err)
	}
	require.NoError(t, nc.Flush())

	gw := &awsgw.NodeDiscovery{
		Expected: 1,
		NATS:     nc,
	}

	result := gw.ActiveNodes(context.Background())
	assert.Equal(t, 2, result)
}

func TestDiscoverActiveNodes_InvalidJSON(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	nc := startDiscoveryNATS(t)

	_, err := nc.Subscribe(clusterv1.NodesDiscoverSubject, func(msg *nats.Msg) {
		msg.Respond([]byte("not json"))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	gw := &awsgw.NodeDiscovery{
		Expected: 4,
		NATS:     nc,
	}

	result := gw.ActiveNodes(context.Background())
	assert.Equal(t, 4, result)
}

func TestDiscoverActiveNodes_DuplicateNodes(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	nc := startDiscoveryNATS(t)

	for range 2 {
		_, err := nc.Subscribe(clusterv1.NodesDiscoverSubject, func(msg *nats.Msg) {
			resp := clusterv1.NodeDiscoverResponse{Node: "same-node"}
			data, _ := json.Marshal(resp)
			msg.Respond(data)
		})
		require.NoError(t, err)
	}
	require.NoError(t, nc.Flush())

	gw := &awsgw.NodeDiscovery{
		Expected: 5,
		NATS:     nc,
	}

	result := gw.ActiveNodes(context.Background())
	assert.Equal(t, 1, result)
}
