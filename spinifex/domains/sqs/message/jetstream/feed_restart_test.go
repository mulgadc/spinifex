package jetstream_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// feedBacklog is the shared-shard history each restart round adds; the
// conformance suite leaves a few hundred appends on shard 0.
const feedBacklog = 1500

// feedStallAfter is the longest a restart round may wait for a change.
const feedStallAfter = 5 * time.Second

// feedProbe reads an exact sequence of changes, recording progress so a
// failure separates a slow replay from an idle or wedged iterator.
type feedProbe struct {
	stallAfter time.Duration
	// onStall, when set, runs at most once, with a snapshot, if no change
	// arrives for stallAfter. expect waits for it before returning.
	onStall func(snapshot string)

	mu        sync.Mutex
	start     time.Time
	done      bool
	elapsed   time.Duration
	delivered int
	last      message.Cursor
	maxGap    time.Duration
	gapAfter  message.Cursor
}

func newFeedProbe() *feedProbe { return &feedProbe{stallAfter: feedStallAfter} }

func (p *feedProbe) snapshot() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := p.elapsed
	if !p.done {
		elapsed = time.Since(p.start)
	}
	return fmt.Sprintf("delivered=%d last_cursor=%d elapsed_ms=%d max_gap_ms=%d max_gap_after_cursor=%d",
		p.delivered, p.last, elapsed.Milliseconds(), p.maxGap.Milliseconds(), p.gapAfter)
}

func (p *feedProbe) gap() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxGap
}

// expect reads len(want) changes and requires each to be the next wanted
// position: every surviving change exactly once, in order.
func (p *feedProbe) expect(f message.Feed, want []message.Position, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	p.mu.Lock()
	p.start, p.done = time.Now(), false
	p.mu.Unlock()
	var stalls sync.WaitGroup
	var once sync.Once
	defer func() {
		stalls.Wait()
		p.mu.Lock()
		p.done, p.elapsed = true, time.Since(p.start)
		p.mu.Unlock()
	}()
	prev := time.Now()
	for i, pos := range want {
		var timer *time.Timer
		if p.onStall != nil {
			stalls.Add(1)
			timer = time.AfterFunc(p.stallAfter, func() {
				defer stalls.Done()
				once.Do(func() { p.onStall(p.snapshot()) })
			})
		}
		c, err := f.Next(ctx)
		if timer != nil && timer.Stop() {
			stalls.Done()
		}
		p.mu.Lock()
		if gap := time.Since(prev); gap > p.maxGap {
			p.maxGap, p.gapAfter = gap, p.last
		}
		if err == nil {
			p.delivered++
			p.last = c.Cursor
		}
		p.mu.Unlock()
		prev = time.Now()
		if err != nil {
			return fmt.Errorf("change %d of %d (want position %d): %w", i+1, len(want), pos, err)
		}
		if c.Position != pos {
			return fmt.Errorf("change %d of %d: got position %d, want %d", i+1, len(want), c.Position, pos)
		}
	}
	return nil
}

func feedRounds() int {
	n, err := strconv.Atoi(os.Getenv("SQS_FEED_RESTART_ROUNDS"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// fillShard appends n bodies across 25 queues on shard 0 and returns their
// positions in order.
func fillShard(t *testing.T, s message.Store, n int) (message.Namespace, []message.Position) {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("fr-%d", time.Now().UnixNano())
	var ns message.Namespace
	positions := make([]message.Position, 0, n)
	for i := range n {
		ns = message.Namespace{Queue: message.QueueID(fmt.Sprintf("%s-%d", prefix, i%25))}
		res, err := s.Append(ctx, ns, message.AppendInput{Body: []byte(strconv.Itoa(i))})
		require.NoError(t, err)
		positions = append(positions, res.Position)
	}
	return ns, positions
}

func appendLive(t *testing.T, s message.Store, ns message.Namespace) message.Position {
	t.Helper()
	res, err := s.Append(context.Background(), ns, message.AppendInput{Body: []byte("live")})
	require.NoError(t, err)
	return res.Position
}

// diagnose logs what the store and server hold when a feed misses a change.
func diagnose(t *testing.T, c *cluster, s message.Store, ns message.Namespace, pos message.Position, snapshot string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Logf("probe: %s want_position=%d", snapshot, pos)
	m, err := s.Get(ctx, ns, pos)
	t.Logf("direct Get of last wanted: err=%v body=%q", err, m.Body)
	stream, err := c.js.Stream(ctx, sqsjs.StreamName(0))
	if err != nil {
		t.Logf("stream: %v", err)
		return
	}
	if info, err := stream.Info(ctx); err == nil {
		leader := ""
		if info.Cluster != nil {
			leader = info.Cluster.Leader
		}
		t.Logf("stream: msgs=%d first=%d last=%d consumers=%d leader=%s replicas=%s",
			info.State.Msgs, info.State.FirstSeq, info.State.LastSeq, info.State.Consumers, leader, replicaState(info.Cluster))
	}
	lister := stream.ListConsumers(ctx)
	for ci := range lister.Info() {
		leader := ""
		if ci.Cluster != nil {
			leader = ci.Cluster.Leader
		}
		t.Logf("consumer %s: delivered_stream=%d pending=%d waiting=%d leader=%s",
			ci.Name, ci.Delivered.Stream, ci.NumPending, ci.NumWaiting, leader)
	}
}

// keepLogs copies the node logs out of the test's temporary directory.
func keepLogs(t *testing.T, c *cluster, round int) {
	dir := os.Getenv("SQS_FEED_RESTART_KEEP_LOGS")
	if dir == "" || c.logDir == "" {
		return
	}
	for i := range 3 {
		name := fmt.Sprintf("sqs-%d.log", i)
		b, err := os.ReadFile(filepath.Join(c.logDir, name))
		if err == nil {
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("round%d-%s", round, name)), b, 0o644)
		}
	}
	t.Logf("node logs kept in %s", dir)
}

func replicaState(ci *jsapi.ClusterInfo) string {
	if ci == nil {
		return "none"
	}
	var parts []string
	for _, r := range ci.Replicas {
		parts = append(parts, fmt.Sprintf("%s current=%t offline=%t lag=%d", r.Name, r.Current, r.Offline, r.Lag))
	}
	return strings.Join(parts, "; ")
}

// stallReporter snapshots the store and server once a probe stalls.
func stallReporter(t *testing.T, c *cluster, s message.Store, ns message.Namespace, want []message.Position, round int) func(string) {
	return func(snapshot string) {
		t.Logf("round %d: no change for %s; snapshot while stalled:", round, feedStallAfter)
		diagnose(t, c, s, ns, want[len(want)-1], snapshot)
		keepLogs(t, c, round)
	}
}

// TestR3FeedOpenedAfterRestart: a feed opened from the start after a full
// cluster restart replays the whole shard once in order, then a live append.
func TestR3FeedOpenedAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a three-node cluster")
	}
	c := startClusterLogged(t, t.TempDir())
	s := c.store(t)
	var shard []message.Position
	for round := range feedRounds() {
		ns, positions := fillShard(t, s, feedBacklog)
		shard = append(shard, positions...)
		c.stop()
		c.start()
		s = c.store(t)

		f, err := s.Feed(context.Background(), 0, 0)
		require.NoError(t, err)
		p := newFeedProbe()
		p.onStall = stallReporter(t, c, s, ns, shard, round)
		require.NoError(t, p.expect(f, shard, 60*time.Second), "round %d: replay after restart", round)
		t.Logf("round %d backlog: %s", round, p.snapshot())
		require.Less(t, p.gap(), feedStallAfter, "round %d: feed idled after restart", round)

		live := appendLive(t, s, ns)
		shard = append(shard, live)
		lp := newFeedProbe()
		require.NoError(t, lp.expect(f, []message.Position{live}, 30*time.Second), "round %d: live append after restart", round)
		require.NoError(t, f.Close())
	}
}

// TestR3FeedOpenAcrossRestart: a feed open before a full cluster restart
// delivers every change once in order across it, then a live append.
func TestR3FeedOpenAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a three-node cluster")
	}
	c := startClusterLogged(t, t.TempDir())
	s := c.store(t)
	var shard []message.Position
	for round := range feedRounds() {
		ns, positions := fillShard(t, s, feedBacklog)
		shard = append(shard, positions...)
		f, err := s.Feed(context.Background(), 0, 0)
		require.NoError(t, err)
		before := len(shard) - feedBacklog*2/3
		pre := newFeedProbe()
		require.NoError(t, pre.expect(f, shard[:before], 30*time.Second), "round %d: reading before restart", round)

		c.stop()
		c.start()
		after := c.store(t)

		p := newFeedProbe()
		p.onStall = stallReporter(t, c, after, ns, shard, round)
		require.NoError(t, p.expect(f, shard[before:], 60*time.Second), "round %d: backlog across restart", round)
		t.Logf("round %d backlog: %s", round, p.snapshot())
		require.Less(t, p.gap(), feedStallAfter, "round %d: feed idled across restart", round)

		live := appendLive(t, after, ns)
		shard = append(shard, live)
		lp := newFeedProbe()
		require.NoError(t, lp.expect(f, []message.Position{live}, 30*time.Second), "round %d: live append across restart", round)
		require.NoError(t, f.Close())
		s = after
	}
}

// TestFeedProbeStallSnapshot drives the probe's stall path: the snapshot runs
// once, sees live elapsed time, and finishes before expect returns.
func TestFeedProbeStallSnapshot(t *testing.T) {
	s, _ := newStore(t)
	f, err := s.Feed(context.Background(), 0, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	p := newFeedProbe()
	p.stallAfter = 50 * time.Millisecond
	var calls int
	var snapshot string
	p.onStall = func(s string) {
		time.Sleep(100 * time.Millisecond)
		calls++
		snapshot = s
	}
	err = p.expect(f, []message.Position{1}, 300*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, calls, "the snapshot runs once and has finished when expect returns")
	assert.Contains(t, snapshot, "delivered=0")
	assert.Regexp(t, `elapsed_ms=([5-9]\d|\d{3,}) `, snapshot, "elapsed is live during a stall")
}
