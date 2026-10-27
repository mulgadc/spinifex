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
	"github.com/stretchr/testify/require"
)

// feedBacklog is the shared-shard history each restart case replays; the
// conformance suite leaves a few hundred appends on shard 0.
const feedBacklog = 1500

// feedProbe reads a feed until one change arrives, recording progress so a
// failure separates a slow replay from an idle or wedged iterator.
type feedProbe struct {
	delivered int
	last      message.Cursor
	maxGap    time.Duration
	gapAfter  message.Cursor
	elapsed   time.Duration
	// onStall, when set, runs once if no change arrives for stallAfter.
	onStall func()
}

const stallAfter = 5 * time.Second

func (p *feedProbe) String() string {
	return fmt.Sprintf("delivered=%d last_cursor=%d elapsed_ms=%d max_gap_ms=%d max_gap_after_cursor=%d",
		p.delivered, p.last, p.elapsed.Milliseconds(), p.maxGap.Milliseconds(), p.gapAfter)
}

func (p *feedProbe) until(f message.Feed, ns message.Namespace, pos message.Position, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start, prev := time.Now(), time.Now()
	defer func() { p.elapsed = time.Since(start) }()
	var once sync.Once
	for {
		var stall *time.Timer
		if p.onStall != nil {
			stall = time.AfterFunc(stallAfter, func() { once.Do(p.onStall) })
		}
		c, err := f.Next(ctx)
		if stall != nil {
			stall.Stop()
		}
		if gap := time.Since(prev); gap > p.maxGap {
			p.maxGap, p.gapAfter = gap, p.last
		}
		prev = time.Now()
		if err != nil {
			return err
		}
		p.delivered++
		p.last = c.Cursor
		if c.Namespace == ns && c.Position == pos {
			return nil
		}
	}
}

func feedRounds(t *testing.T) int {
	n, err := strconv.Atoi(os.Getenv("SQS_FEED_RESTART_ROUNDS"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// fillShard appends n bodies across 25 queues on shard 0 and returns the
// last one, so a feed from the start must replay the whole backlog first.
func fillShard(t *testing.T, s message.Store, n int) (message.Namespace, message.Position) {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("fr-%d", time.Now().UnixNano())
	var ns message.Namespace
	var pos message.Position
	for i := range n {
		ns = message.Namespace{Queue: message.QueueID(fmt.Sprintf("%s-%d", prefix, i%25))}
		res, err := s.Append(ctx, ns, message.AppendInput{Body: []byte(strconv.Itoa(i))})
		require.NoError(t, err)
		pos = res.Position
	}
	return ns, pos
}

// diagnose logs what the store and server hold when a feed misses its target.
func diagnose(t *testing.T, c *cluster, s message.Store, ns message.Namespace, pos message.Position, p *feedProbe) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Logf("probe: %s want_position=%d", p, pos)
	m, err := s.Get(ctx, ns, pos)
	t.Logf("direct Get: err=%v body=%q", err, m.Body)
	stream, err := c.js.Stream(ctx, sqsjs.StreamName(0))
	if err != nil {
		t.Logf("stream: %v", err)
		return
	}
	info, err := stream.Info(ctx)
	if err == nil {
		t.Logf("stream: msgs=%d first=%d last=%d consumers=%d leader=%s replicas=%s",
			info.State.Msgs, info.State.FirstSeq, info.State.LastSeq, info.State.Consumers, info.Cluster.Leader, replicaState(info.Cluster))
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
	if c.logDir == "" {
		return
	}
	for i := range 3 {
		b, err := os.ReadFile(filepath.Join(c.logDir, fmt.Sprintf("sqs-%d.log", i)))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		t.Logf("sqs-%d log tail:\n%s", i, strings.Join(lines[max(0, len(lines)-25):], "\n"))
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

func appendLive(t *testing.T, s message.Store, ns message.Namespace) message.Position {
	t.Helper()
	res, err := s.Append(context.Background(), ns, message.AppendInput{Body: []byte("live")})
	require.NoError(t, err)
	return res.Position
}

// TestR3FeedOpenedAfterRestart: a feed opened from the start after a full
// cluster restart replays a shared-shard backlog, then sees a live append.
func TestR3FeedOpenedAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a three-node cluster")
	}
	c := startClusterLogged(t, t.TempDir())
	s := c.store(t)
	for round := range feedRounds(t) {
		ns, pos := fillShard(t, s, feedBacklog)
		c.stop()
		c.start()
		s = c.store(t)

		f, err := s.Feed(context.Background(), 0, 0)
		require.NoError(t, err)
		p := &feedProbe{}
		p.onStall = func() {
			t.Logf("round %d: no change for %s; snapshot while stalled:", round, stallAfter)
			diagnose(t, c, s, ns, pos, p)
			keepLogs(t, c, round)
		}
		if err := p.until(f, ns, pos, 60*time.Second); err != nil {
			diagnose(t, c, s, ns, pos, p)
			t.Fatalf("round %d: backlog replay after restart: %v", round, err)
		}
		t.Logf("round %d backlog: %s", round, p)
		require.Less(t, p.maxGap, stallAfter, "round %d: feed idled after restart", round)

		live := appendLive(t, s, ns)
		lp := &feedProbe{}
		if err := lp.until(f, ns, live, 30*time.Second); err != nil {
			diagnose(t, c, s, ns, live, lp)
			t.Fatalf("round %d: live append after restart: %v", round, err)
		}
		t.Logf("round %d live: %s", round, lp)
		require.NoError(t, f.Close())
	}
}

// TestR3FeedOpenAcrossRestart: a feed open before a full cluster restart
// resumes its backlog afterwards and then sees a live append.
func TestR3FeedOpenAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a three-node cluster")
	}
	c := startClusterLogged(t, t.TempDir())
	s := c.store(t)
	for round := range feedRounds(t) {
		ns, pos := fillShard(t, s, feedBacklog)
		f, err := s.Feed(context.Background(), 0, 0)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		for range feedBacklog / 3 {
			_, err := f.Next(ctx)
			require.NoError(t, err, "round %d: reading before restart", round)
		}
		cancel()

		c.stop()
		c.start()
		after := c.store(t)

		p := &feedProbe{}
		p.onStall = func() {
			t.Logf("round %d: no change for %s; snapshot while stalled:", round, stallAfter)
			diagnose(t, c, after, ns, pos, p)
			keepLogs(t, c, round)
		}
		if err := p.until(f, ns, pos, 60*time.Second); err != nil {
			diagnose(t, c, after, ns, pos, p)
			t.Fatalf("round %d: backlog across restart: %v", round, err)
		}
		t.Logf("round %d backlog: %s", round, p)
		require.Less(t, p.maxGap, stallAfter, "round %d: feed idled across restart", round)

		live := appendLive(t, after, ns)
		lp := &feedProbe{}
		if err := lp.until(f, ns, live, 30*time.Second); err != nil {
			diagnose(t, c, after, ns, live, lp)
			t.Fatalf("round %d: live append across restart: %v", round, err)
		}
		t.Logf("round %d live: %s", round, lp)
		require.NoError(t, f.Close())
		s = after
	}
}
