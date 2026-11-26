package awsgw_test

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"
	"github.com/nats-io/nats.go"
)

// answerAs answers the discovery subject as one node.
func answerAs(t *testing.T, nc *nats.Conn, node string, onRound func()) *nats.Subscription {
	t.Helper()
	sub, err := nc.Subscribe(clusterv1.NodesDiscoverSubject, func(msg *nats.Msg) {
		if onRound != nil {
			onRound()
		}
		payload, err := json.Marshal(clusterv1.NodeDiscoverResponse{Node: node})
		if err != nil {
			return
		}
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())
	return sub
}

// respondAsNodes answers the discovery subject as n distinct nodes and reports
// how many discovery rounds were served.
func respondAsNodes(t *testing.T, nc *nats.Conn, n int) *atomic.Int64 {
	t.Helper()
	var rounds atomic.Int64
	for i := range n {
		var onRound func()
		if i == 0 {
			onRound = func() { rounds.Add(1) }
		}
		answerAs(t, nc, string(rune('a'+i)), onRound)
	}
	return &rounds
}

// runDiscovery starts Run and returns a stop that waits for it to return. The
// cleanup stops it before the test's timing overrides are restored.
func runDiscovery(t *testing.T, d *awsgw.NodeDiscovery) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func requireActiveNodes(t *testing.T, d *awsgw.NodeDiscovery, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return d.ActiveNodes(context.Background()) == want },
		2*time.Second, 5*time.Millisecond, "ActiveNodes never reached %d", want)
}

// The gather counts whoever answers and so always sits out its whole timeout.
// The count must follow a grow and a shrink while every read stays well clear
// of that timeout.
func TestDiscoverActiveNodesKeepsItsCostOffTheRequestPath(t *testing.T) {
	const timeout = 50 * time.Millisecond
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, timeout)
	awsgw.SetActiveNodesTTLForTest(t, timeout)
	_, nc := testutil.StartTestNATS(t)
	respondAsNodes(t, nc, 3)

	gw := &awsgw.NodeDiscovery{NATS: nc, Expected: 1}
	runDiscovery(t, gw)

	var slowest time.Duration
	waitFor := func(want int) {
		t.Helper()
		require.Eventually(t, func() bool {
			start := time.Now()
			got := gw.ActiveNodes(context.Background())
			slowest = max(slowest, time.Since(start))
			return got == want
		}, 2*time.Second, 5*time.Millisecond, "ActiveNodes never reached %d", want)
	}

	waitFor(3)
	fourth := answerAs(t, nc, "d", nil)
	waitFor(4)
	require.NoError(t, fourth.Unsubscribe())
	waitFor(3)

	if slowest >= timeout {
		t.Errorf("slowest ActiveNodes call took %v, want under the %v gather timeout", slowest, timeout)
	}
}

// Membership does not change per request, so a read must reuse the last
// refresh and publish nothing.
func TestDiscoverActiveNodesReusesARecentAnswer(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	awsgw.SetActiveNodesTTLForTest(t, time.Hour)
	_, nc := testutil.StartTestNATS(t)
	rounds := respondAsNodes(t, nc, 3)

	gw := &awsgw.NodeDiscovery{NATS: nc, Expected: 1}
	runDiscovery(t, gw)
	requireActiveNodes(t, gw, 3)
	first := rounds.Load()

	for range 5 {
		if count := gw.ActiveNodes(context.Background()); count != 3 {
			t.Fatalf("cached discovery = %d, want 3", count)
		}
	}

	if got := rounds.Load(); got != first {
		t.Errorf("discovery ran %d more time(s) between refreshes, want the stored answer reused", got-first)
	}
}

// A fallback is not evidence of anything: keeping one would make a momentary
// NATS problem outlive itself.
func TestDiscoverActiveNodesDoesNotCacheAFallback(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	awsgw.SetActiveNodesTTLForTest(t, 50*time.Millisecond)
	_, nc := testutil.StartTestNATS(t)

	gw := &awsgw.NodeDiscovery{NATS: nc, Expected: 2}
	runDiscovery(t, gw)

	if count := gw.ActiveNodes(context.Background()); count != 2 {
		t.Fatalf("fallback = %d, want the configured node count", count)
	}

	respondAsNodes(t, nc, 3)
	requireActiveNodes(t, gw, 3)
}

// When every node stops answering, the next refresh must drop the old count
// and fall back to the configured node count.
func TestDiscoverActiveNodesClearsTheCountWhenNoNodeAnswers(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	awsgw.SetActiveNodesTTLForTest(t, 50*time.Millisecond)
	_, nc := testutil.StartTestNATS(t)
	var subs []*nats.Subscription
	for _, node := range []string{"a", "b", "c"} {
		subs = append(subs, answerAs(t, nc, node, nil))
	}

	gw := &awsgw.NodeDiscovery{NATS: nc, Expected: 2}
	runDiscovery(t, gw)
	requireActiveNodes(t, gw, 3)

	for _, sub := range subs {
		require.NoError(t, sub.Unsubscribe())
	}
	require.NoError(t, nc.Flush())
	requireActiveNodes(t, gw, 2)
}

func startDiscoveryNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	return nc
}

func TestDiscoverActiveNodes_NilNATS(t *testing.T) {
	t.Parallel()
	gw := &awsgw.NodeDiscovery{
		Expected: 3,
		NATS:     nil,
	}

	result := gw.DiscoverNow(context.Background())
	assert.Equal(t, 3, result)
}

func TestDiscoverActiveNodes_NoResponders(t *testing.T) {
	awsgw.SetDiscoverActiveNodesTimeoutForTest(t, 50*time.Millisecond)
	nc := startDiscoveryNATS(t)

	gw := &awsgw.NodeDiscovery{
		Expected: 5,
		NATS:     nc,
	}

	result := gw.DiscoverNow(context.Background())
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

	result := gw.DiscoverNow(context.Background())
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

	result := gw.DiscoverNow(context.Background())
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

	result := gw.DiscoverNow(context.Background())
	assert.Equal(t, 1, result)
}
