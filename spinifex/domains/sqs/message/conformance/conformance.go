// Package conformance is the definition of a correct SQS message store
// (ADR-0011:S6). It drives message.Store only through the interface, so the
// same cases run unmodified against every implementation.
package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Harness is one store under test and the hooks its implementation can offer.
type Harness struct {
	Store message.Store
	// MessageShards and StateShards are how many of each allocation the
	// store serves, numbered from zero; the cross-shard case needs two
	// message shards.
	MessageShards int
	StateShards   int
	// Reopen discards Store without closing it, as a crash would, and
	// returns a store over the same durable data. Nil skips the crash cases.
	Reopen func(t *testing.T) message.Store
	// Advance moves the store's clock forward. Nil skips the backstop case.
	Advance func(d time.Duration)
}

// Factory builds a fresh harness for one case.
type Factory func(t *testing.T) Harness

// feedWait bounds how long a case waits for a change it expects.
const feedWait = 5 * time.Second

// Run executes every contract case against stores built by newHarness. Cases
// use fresh queue IDs, so one long-lived store may back every case.
func Run(t *testing.T, newHarness Factory) {
	cases := []struct {
		name string
		run  func(t *testing.T, h Harness)
	}{
		{"Limits", testLimits},
		{"InvalidNamespaceRejected", testInvalidNamespace},
		{"QueueCreationNeedsNoStoreCall", testNoCreateCall},
		{"AppendAndGet", testAppendAndGet},
		{"GetMissing", testGetMissing},
		{"DeleteIsIdempotentAndPositionsAreNotReused", testDelete},
		{"DeleteThrough", testDeleteThrough},
		{"IdempotentAppend", testIdempotentAppend},
		{"ConditionalState", testConditionalState},
		{"StateOutlivesBody", testStateOutlivesBody},
		{"ConcurrentConditionalWrites", testConcurrentConditionalWrites},
		{"FeedOrderAndResume", testFeedOrderAndResume},
		{"FeedWakesOnLiveAppend", testFeedLive},
		{"FeedOmitsDeletedAppends", testFeedOmitsDeleted},
		{"FeedHonoursContext", testFeedContext},
		{"ScanPagesInOrder", testScanPagesInOrder},
		{"ScanAdvancesAcrossDeletedPositions", testScanAcrossDeleted},
		{"ScanExcludesAppendsAfterBound", testScanBound},
		{"ScanRejectsBadInput", testScanBadInput},
		{"NamespaceIsolation", testNamespaceIsolation},
		{"ShardIsolation", testShardIsolation},
		{"CountAndEnumerate", testCountAndEnumerate},
		{"EnumerateStateOnlyNamespace", testEnumerateStateOnly},
		{"DeleteNamespace", testDeleteNamespace},
		{"CrashBetweenAppendAndStateWrite", testCrashAfterAppend},
		{"CrashBetweenTombstoneAndBodyDelete", testCrashAfterTombstone},
		{"IdempotentAppendReplayAfterCrash", testIdempotentReplayAfterCrash},
		{"PositionsSurviveCrash", testPositionsSurviveCrash},
		{"RetentionBackstop", testRetentionBackstop},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			require.NotNil(t, h.Store, "harness returned no store")
			c.run(t, h)
		})
	}
}

var nsCounter atomic.Uint64

// newNamespace returns a queue ID no other case or run has used, so a shared
// store needs no cleanup between cases.
func newNamespace(t *testing.T, messageShard message.Shard) message.Namespace {
	t.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return message.Namespace{
		MessageShard: messageShard,
		Queue:        message.QueueID(fmt.Sprintf("cf-%s-%d", hex.EncodeToString(b[:]), nsCounter.Add(1))),
	}
}

func appendBody(t *testing.T, s message.Store, ns message.Namespace, body string) message.Position {
	t.Helper()
	res, err := s.Append(context.Background(), ns, message.AppendInput{Body: []byte(body), Meta: map[string]string{"body": body}})
	require.NoError(t, err)
	require.False(t, res.Duplicate, "an append without an idempotency key is never a duplicate")
	require.NotZero(t, res.Position, "positions are non-zero")
	return res.Position
}

func requireCount(t *testing.T, s message.Store, ns message.Namespace, want message.Usage) {
	t.Helper()
	got, err := s.Count(context.Background(), ns)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func testLimits(t *testing.T, h Harness) {
	l := h.Store.Limits()
	assert.GreaterOrEqual(t, l.RetentionBackstop, message.MinRetentionBackstop, "ADR-0011:S12: the backstop is beyond the longest queue retention")
	assert.Positive(t, l.IdempotencyWindow, "idempotent append needs a window")
}

func testInvalidNamespace(t *testing.T, h Harness) {
	ctx := context.Background()
	for _, id := range []string{"", "a.b", "a*", "a>", "a b", "a/b", strings.Repeat("q", message.MaxQueueIDLen+1)} {
		ns := message.Namespace{Queue: message.QueueID(id)}
		_, err := h.Store.Append(ctx, ns, message.AppendInput{Body: []byte("x")})
		assert.ErrorIsf(t, err, message.ErrInvalidArgument, "Append queue ID %q", id)
		_, err = h.Store.Get(ctx, ns, 1)
		assert.ErrorIsf(t, err, message.ErrInvalidArgument, "Get queue ID %q", id)
		_, err = h.Store.PutState(ctx, ns, 1, []byte("x"), 0)
		assert.ErrorIsf(t, err, message.ErrInvalidArgument, "PutState queue ID %q", id)
		_, err = h.Store.Count(ctx, ns)
		assert.ErrorIsf(t, err, message.ErrInvalidArgument, "Count queue ID %q", id)
		assert.ErrorIsf(t, h.Store.DeleteNamespace(ctx, ns), message.ErrInvalidArgument, "DeleteNamespace queue ID %q", id)
	}
}

// testNoCreateCall holds ADR-0011:S9: creating a queue is the domain recording
// its shard and queue ID, so a namespace's first append needs no setup.
func testNoCreateCall(t *testing.T, h Harness) {
	ns := newNamespace(t, 0)
	requireCount(t, h.Store, ns, message.Usage{})
	appendBody(t, h.Store, ns, "first")
	requireCount(t, h.Store, ns, message.Usage{Messages: 1})
}

func testAppendAndGet(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	var last message.Position
	for i := range 5 {
		body := fmt.Sprintf("body-%d", i)
		pos := appendBody(t, h.Store, ns, body)
		require.Greater(t, pos, last, "positions strictly increase in append order")
		last = pos

		got, err := h.Store.Get(ctx, ns, pos)
		require.NoError(t, err)
		assert.Equal(t, pos, got.Position)
		assert.Equal(t, []byte(body), got.Body)
		assert.Equal(t, map[string]string{"body": body}, got.Meta)
	}
	requireCount(t, h.Store, ns, message.Usage{Messages: 5})
}

func testGetMissing(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	_, err := h.Store.Get(ctx, ns, 1)
	require.ErrorIs(t, err, message.ErrNotFound)
	_, err = h.Store.GetState(ctx, ns, 1)
	require.ErrorIs(t, err, message.ErrNotFound)
}

func testDelete(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	first := appendBody(t, h.Store, ns, "a")
	second := appendBody(t, h.Store, ns, "b")

	require.NoError(t, h.Store.Delete(ctx, ns, first))
	_, err := h.Store.Get(ctx, ns, first)
	require.ErrorIs(t, err, message.ErrNotFound)
	require.NoError(t, h.Store.Delete(ctx, ns, first), "deleting an absent message succeeds")
	requireCount(t, h.Store, ns, message.Usage{Messages: 1})

	_, err = h.Store.Get(ctx, ns, second)
	require.NoError(t, err, "deleting one message leaves the others")

	third := appendBody(t, h.Store, ns, "c")
	require.Greater(t, third, second, "a position is never reused after deletion")
}

func testDeleteThrough(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	var pos []message.Position
	for i := range 5 {
		pos = append(pos, appendBody(t, h.Store, ns, fmt.Sprintf("m%d", i)))
	}
	_, err := h.Store.PutState(ctx, ns, pos[0], []byte("claimed"), 0)
	require.NoError(t, err)

	require.NoError(t, h.Store.DeleteThrough(ctx, ns, pos[2]))
	for i, p := range pos {
		_, err := h.Store.Get(ctx, ns, p)
		if i <= 2 {
			assert.ErrorIsf(t, err, message.ErrNotFound, "position %d is at or before the range end", p)
		} else {
			assert.NoErrorf(t, err, "position %d is after the range end", p)
		}
	}
	_, err = h.Store.GetState(ctx, ns, pos[0])
	require.NoError(t, err, "a range delete removes messages, not state records")
	requireCount(t, h.Store, ns, message.Usage{Messages: 2, States: 1})
}

func testIdempotentAppend(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	other := newNamespace(t, 0)

	first, err := h.Store.Append(ctx, ns, message.AppendInput{Body: []byte("original"), IdempotencyKey: "move-1"})
	require.NoError(t, err)
	require.False(t, first.Duplicate)

	again, err := h.Store.Append(ctx, ns, message.AppendInput{Body: []byte("retried"), IdempotencyKey: "move-1"})
	require.NoError(t, err)
	assert.True(t, again.Duplicate, "a repeated key is reported as a duplicate")
	assert.Equal(t, first.Position, again.Position, "a repeated key returns the first position")
	got, err := h.Store.Get(ctx, ns, first.Position)
	require.NoError(t, err)
	assert.Equal(t, []byte("original"), got.Body, "a repeated key stores nothing new")
	requireCount(t, h.Store, ns, message.Usage{Messages: 1})

	elsewhere, err := h.Store.Append(ctx, other, message.AppendInput{Body: []byte("x"), IdempotencyKey: "move-1"})
	require.NoError(t, err)
	assert.False(t, elsewhere.Duplicate, "idempotency keys are scoped to a namespace")
	requireCount(t, h.Store, other, message.Usage{Messages: 1})
}

func testConditionalState(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	pos := appendBody(t, h.Store, ns, "m")

	r1, err := h.Store.PutState(ctx, ns, pos, []byte("v1"), 0)
	require.NoError(t, err)
	require.NotZero(t, r1)
	_, err = h.Store.PutState(ctx, ns, pos, []byte("again"), 0)
	require.ErrorIs(t, err, message.ErrRevisionMismatch, "creating an existing record fails")

	r2, err := h.Store.PutState(ctx, ns, pos, []byte("v2"), r1)
	require.NoError(t, err)
	require.NotEqual(t, r1, r2)
	_, err = h.Store.PutState(ctx, ns, pos, []byte("stale"), r1)
	require.ErrorIs(t, err, message.ErrRevisionMismatch, "a stale revision is rejected")

	got, err := h.Store.GetState(ctx, ns, pos)
	require.NoError(t, err)
	assert.Equal(t, message.State{Value: []byte("v2"), Revision: r2}, got)

	require.ErrorIs(t, h.Store.DeleteState(ctx, ns, pos, r1), message.ErrRevisionMismatch)
	require.NoError(t, h.Store.DeleteState(ctx, ns, pos, r2))
	_, err = h.Store.GetState(ctx, ns, pos)
	require.ErrorIs(t, err, message.ErrNotFound)
	require.ErrorIs(t, h.Store.DeleteState(ctx, ns, pos, r2), message.ErrNotFound)
	requireCount(t, h.Store, ns, message.Usage{Messages: 1})
}

// testStateOutlivesBody: a tombstone is written before the body is deleted,
// so a state record must stand on its own.
func testStateOutlivesBody(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	pos := appendBody(t, h.Store, ns, "m")
	rev, err := h.Store.PutState(ctx, ns, pos, []byte("tombstone"), 0)
	require.NoError(t, err)
	require.NoError(t, h.Store.Delete(ctx, ns, pos))

	got, err := h.Store.GetState(ctx, ns, pos)
	require.NoError(t, err)
	assert.Equal(t, rev, got.Revision)
	requireCount(t, h.Store, ns, message.Usage{States: 1})
}

func testConcurrentConditionalWrites(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	pos := appendBody(t, h.Store, ns, "m")
	const writers = 16

	race := func(expected message.Revision) message.Revision {
		var wg sync.WaitGroup
		var wins atomic.Int32
		var won atomic.Uint64
		errs := make(chan error, writers)
		for i := range writers {
			wg.Go(func() {
				rev, err := h.Store.PutState(ctx, ns, pos, fmt.Appendf(nil, "writer-%d", i), expected)
				switch {
				case err == nil:
					wins.Add(1)
					won.Store(uint64(rev))
				case !errors.Is(err, message.ErrRevisionMismatch):
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "a losing conditional write fails only with ErrRevisionMismatch")
		}
		require.EqualValues(t, 1, wins.Load(), "exactly one of %d writers expecting revision %d wins", writers, expected)
		return message.Revision(won.Load())
	}

	created := race(0)
	updated := race(created)
	got, err := h.Store.GetState(ctx, ns, pos)
	require.NoError(t, err)
	assert.Equal(t, updated, got.Revision)
}

// nextIn reads the feed until a change in ns arrives, skipping other
// namespaces sharing the shard.
func nextIn(t *testing.T, f message.Feed, ns message.Namespace) message.Change {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), feedWait)
	defer cancel()
	for {
		c, err := f.Next(ctx)
		require.NoError(t, err, "waiting for a change in %s", ns.Queue)
		if c.Namespace == ns {
			return c
		}
	}
}

func openFeed(t *testing.T, s message.Store, messageShard message.Shard, after message.Cursor) message.Feed {
	t.Helper()
	f, err := s.Feed(context.Background(), messageShard, after)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func testFeedOrderAndResume(t *testing.T, h Harness) {
	ns := newNamespace(t, 0)
	var pos []message.Position
	for i := range 3 {
		pos = append(pos, appendBody(t, h.Store, ns, fmt.Sprintf("m%d", i)))
	}

	f := openFeed(t, h.Store, 0, 0)
	var changes []message.Change
	for range pos {
		changes = append(changes, nextIn(t, f, ns))
	}
	for i, c := range changes {
		assert.Equal(t, pos[i], c.Position, "changes arrive in position order")
		assert.Equal(t, map[string]string{"body": fmt.Sprintf("m%d", i)}, c.Meta, "the feed carries metadata")
	}

	resumed := openFeed(t, h.Store, 0, changes[1].Cursor)
	assert.Equal(t, pos[2], nextIn(t, resumed, ns).Position, "a cursor resumes after its change")
}

func testFeedLive(t *testing.T, h Harness) {
	ns := newNamespace(t, 0)
	f := openFeed(t, h.Store, 0, 0)
	pos := appendBody(t, h.Store, ns, "late")
	assert.Equal(t, pos, nextIn(t, f, ns).Position, "an open feed sees an append made after it opened")
}

func testFeedOmitsDeleted(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	gone := appendBody(t, h.Store, ns, "gone")
	kept := appendBody(t, h.Store, ns, "kept")
	require.NoError(t, h.Store.Delete(ctx, ns, gone))

	f := openFeed(t, h.Store, 0, 0)
	c := nextIn(t, f, ns)
	if c.Position == gone {
		c = nextIn(t, f, ns)
	}
	assert.Equal(t, kept, c.Position, "a deleted append never hides a later one")
}

func testFeedContext(t *testing.T, h Harness) {
	f := openFeed(t, h.Store, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for {
		_, err := f.Next(ctx)
		if err != nil {
			require.ErrorIs(t, err, context.Canceled)
			return
		}
	}
}

func testNamespaceIsolation(t *testing.T, h Harness) {
	ctx := context.Background()
	a, b := newNamespace(t, 0), newNamespace(t, 0)
	pa := appendBody(t, h.Store, a, "a")
	_, err := h.Store.PutState(ctx, a, pa, []byte("a-state"), 0)
	require.NoError(t, err)

	_, err = h.Store.Get(ctx, b, pa)
	require.ErrorIs(t, err, message.ErrNotFound, "a namespace never reads another's message")
	_, err = h.Store.GetState(ctx, b, pa)
	require.ErrorIs(t, err, message.ErrNotFound, "a namespace never reads another's state")
	requireCount(t, h.Store, b, message.Usage{})

	pb := appendBody(t, h.Store, b, "b")
	require.NoError(t, h.Store.DeleteNamespace(ctx, a))
	_, err = h.Store.Get(ctx, b, pb)
	require.NoError(t, err, "deleting one namespace leaves its shard neighbours")
}

func testShardIsolation(t *testing.T, h Harness) {
	if h.MessageShards < 2 {
		t.Skip("store serves one message shard")
	}
	ctx := context.Background()
	a := newNamespace(t, 0)
	b := message.Namespace{MessageShard: 1, Queue: a.Queue}
	if h.StateShards >= 2 {
		b.StateShard = 1
	}
	pa := appendBody(t, h.Store, a, "a")
	_, err := h.Store.PutState(ctx, a, pa, []byte("a"), 0)
	require.NoError(t, err)
	_, err = h.Store.Get(ctx, b, pa)
	require.ErrorIs(t, err, message.ErrNotFound, "the same queue ID on another shard is another namespace")
	_, err = h.Store.GetState(ctx, b, pa)
	require.ErrorIs(t, err, message.ErrNotFound, "state is scoped to the whole namespace")
	requireCount(t, h.Store, b, message.Usage{})

	f := openFeed(t, h.Store, 1, 0)
	pb := appendBody(t, h.Store, b, "b")
	assert.Equal(t, pb, nextIn(t, f, b).Position, "a shard's feed carries only that shard's appends")
}

// otherStateShard is a state shard other than zero when the store serves
// one, so a store that drops or zeroes StateShard is caught.
func otherStateShard(h Harness) message.Shard {
	if h.StateShards >= 2 {
		return message.Shard(safecast.IntToUint32(h.StateShards - 1))
	}
	return 0
}

func enumerate(t *testing.T, s message.Store, messageShard message.Shard) map[message.Namespace]message.Usage {
	t.Helper()
	listed, err := s.Namespaces(context.Background(), messageShard)
	require.NoError(t, err)
	found := map[message.Namespace]message.Usage{}
	for _, n := range listed {
		found[n.Namespace] = n.Usage
	}
	return found
}

func testCountAndEnumerate(t *testing.T, h Harness) {
	ctx := context.Background()
	ns, unused := newNamespace(t, 0), newNamespace(t, 0)
	ns.StateShard = otherStateShard(h)
	p := appendBody(t, h.Store, ns, "a")
	appendBody(t, h.Store, ns, "b")
	_, err := h.Store.PutState(ctx, ns, p, []byte("s"), 0)
	require.NoError(t, err)
	requireCount(t, h.Store, ns, message.Usage{Messages: 2, States: 1})

	found := enumerate(t, h.Store, 0)
	assert.Equal(t, message.Usage{Messages: 2, States: 1}, found[ns], "enumeration reports the complete namespace and its usage")
	for n := range found {
		assert.NotEqual(t, unused.Queue, n.Queue, "a namespace never written is not enumerated")
	}
}

// testEnumerateStateOnly: after a tombstone and the body delete, only state
// remains, and enumeration must still name its exact allocations so a
// caller without the queue record can count and clean it.
func testEnumerateStateOnly(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	ns.StateShard = otherStateShard(h)
	p := appendBody(t, h.Store, ns, "m")
	_, err := h.Store.PutState(ctx, ns, p, []byte("tombstone"), 0)
	require.NoError(t, err)
	require.NoError(t, h.Store.Delete(ctx, ns, p))

	found := enumerate(t, h.Store, 0)
	require.Contains(t, found, ns, "a state-only namespace is enumerated with its exact StateShard")
	assert.Equal(t, message.Usage{States: 1}, found[ns])
}

func testDeleteNamespace(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	p := appendBody(t, h.Store, ns, "a")
	appendBody(t, h.Store, ns, "b")
	_, err := h.Store.PutState(ctx, ns, p, []byte("s"), 0)
	require.NoError(t, err)

	require.NoError(t, h.Store.DeleteNamespace(ctx, ns))
	requireCount(t, h.Store, ns, message.Usage{})
	_, err = h.Store.GetState(ctx, ns, p)
	require.ErrorIs(t, err, message.ErrNotFound, "namespace deletion removes state records")
	listed, err := h.Store.Namespaces(ctx, 0)
	require.NoError(t, err)
	for _, n := range listed {
		assert.NotEqual(t, ns.Queue, n.Namespace.Queue, "a deleted namespace is not enumerated")
	}
	require.NoError(t, h.Store.DeleteNamespace(ctx, ns), "namespace deletion is idempotent")
}

func requireReopen(t *testing.T, h Harness) message.Store {
	t.Helper()
	if h.Reopen == nil {
		t.Skip("harness cannot simulate a crash")
	}
	s := h.Reopen(t)
	require.NotNil(t, s)
	return s
}

// testCrashAfterAppend: the domain appends, then writes the message's state;
// a crash between them leaves a durable message with no state, which the
// feed still offers so the domain can recover it.
func testCrashAfterAppend(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	pos := appendBody(t, h.Store, ns, "orphan")
	s := requireReopen(t, h)

	got, err := s.Get(ctx, ns, pos)
	require.NoError(t, err, "an acknowledged append survives a crash")
	assert.Equal(t, []byte("orphan"), got.Body)
	_, err = s.GetState(ctx, ns, pos)
	require.ErrorIs(t, err, message.ErrNotFound)
	assert.Equal(t, pos, nextIn(t, openFeed(t, s, 0, 0), ns).Position, "the feed replays the stateless message")
	_, err = s.PutState(ctx, ns, pos, []byte("recovered"), 0)
	require.NoError(t, err, "recovery creates the missing state")
}

// testCrashAfterTombstone: the domain tombstones, then deletes the body; a
// crash between them must leave the tombstone and allow the delete to replay.
func testCrashAfterTombstone(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	pos := appendBody(t, h.Store, ns, "m")
	rev, err := h.Store.PutState(ctx, ns, pos, []byte("tombstone"), 0)
	require.NoError(t, err)
	s := requireReopen(t, h)

	st, err := s.GetState(ctx, ns, pos)
	require.NoError(t, err, "an acknowledged state write survives a crash")
	assert.Equal(t, message.State{Value: []byte("tombstone"), Revision: rev}, st)
	_, err = s.Get(ctx, ns, pos)
	require.NoError(t, err, "the body is still there to delete")

	require.NoError(t, s.Delete(ctx, ns, pos))
	require.NoError(t, s.Delete(ctx, ns, pos), "a replayed body delete succeeds")
	require.NoError(t, s.DeleteState(ctx, ns, pos, rev))
	requireCount(t, s, ns, message.Usage{})
}

func testIdempotentReplayAfterCrash(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	first, err := h.Store.Append(ctx, ns, message.AppendInput{Body: []byte("copy"), IdempotencyKey: "redrive-7"})
	require.NoError(t, err)
	s := requireReopen(t, h)

	again, err := s.Append(ctx, ns, message.AppendInput{Body: []byte("copy"), IdempotencyKey: "redrive-7"})
	require.NoError(t, err)
	assert.True(t, again.Duplicate, "an idempotency key survives a crash")
	assert.Equal(t, first.Position, again.Position)
	requireCount(t, s, ns, message.Usage{Messages: 1})
}

func testPositionsSurviveCrash(t *testing.T, h Harness) {
	ns := newNamespace(t, 0)
	before := appendBody(t, h.Store, ns, "before")
	s := requireReopen(t, h)
	after := appendBody(t, s, ns, "after")
	require.Greater(t, after, before, "positions keep increasing across a crash")
}

func testRetentionBackstop(t *testing.T, h Harness) {
	if h.Advance == nil {
		t.Skip("harness cannot advance the store's clock")
	}
	ctx := context.Background()
	backstop := h.Store.Limits().RetentionBackstop
	ns := newNamespace(t, 0)
	old := appendBody(t, h.Store, ns, "old")
	h.Advance(backstop - time.Hour)
	young := appendBody(t, h.Store, ns, "young")
	h.Advance(2 * time.Hour)

	_, err := h.Store.Get(ctx, ns, old)
	require.ErrorIs(t, err, message.ErrNotFound, "a message past the backstop is dropped")
	_, err = h.Store.Get(ctx, ns, young)
	require.NoError(t, err, "a message inside the backstop is kept")
}

// scanPass runs one complete scan pass of shard at limit per page, failing
// on any duplicate or out-of-order position, and returns every change.
func scanPass(t *testing.T, s message.Store, shard message.Shard, limit int, afterFirst func()) []message.Change {
	t.Helper()
	var (
		all  []message.Change
		from message.ScanCursor
		last message.Cursor
	)
	for page := 0; ; page++ {
		require.Less(t, page, 10_000, "scan pass never finished")
		got, err := s.Scan(context.Background(), shard, from, limit)
		require.NoError(t, err)
		require.LessOrEqual(t, len(got.Changes), limit)
		if page > 0 {
			require.Equal(t, from.Bound, got.Next.Bound, "a pass keeps its bound")
		}
		for _, c := range got.Changes {
			require.Greater(t, c.Cursor, last, "positions come once each, in order")
			require.LessOrEqual(t, c.Cursor, got.Next.Bound, "nothing after the bound")
			last = c.Cursor
		}
		all = append(all, got.Changes...)
		if got.Done {
			require.Equal(t, got.Next.Bound, got.Next.After)
			return all
		}
		require.Greater(t, got.Next.After, from.After, "an unfinished page advances")
		from = got.Next
		if page == 0 && afterFirst != nil {
			afterFirst()
		}
	}
}

func positionsIn(changes []message.Change, nss ...message.Namespace) []message.Position {
	want := map[message.Namespace]bool{}
	for _, ns := range nss {
		want[ns] = true
	}
	var out []message.Position
	for _, c := range changes {
		if want[c.Namespace] {
			out = append(out, c.Position)
		}
	}
	return out
}

func testScanPagesInOrder(t *testing.T, h Harness) {
	a, b := newNamespace(t, 0), newNamespace(t, 0)
	b.StateShard = otherStateShard(h)
	var want []message.Position
	for i := range 7 {
		ns := a
		if i%2 == 1 {
			ns = b
		}
		want = append(want, appendBody(t, h.Store, ns, fmt.Sprintf("m%d", i)))
	}
	all := scanPass(t, h.Store, 0, 2, nil)
	assert.Equal(t, want, positionsIn(all, a, b), "every surviving append, once, in order")
	for _, c := range all {
		if c.Namespace == b {
			assert.Equal(t, b.StateShard, c.Namespace.StateShard, "complete namespace")
			assert.Equal(t, map[string]string{"body": c.Meta["body"]}, c.Meta, "domain metadata only")
		}
	}
}

func testScanAcrossDeleted(t *testing.T, h Harness) {
	ctx := context.Background()
	ns := newNamespace(t, 0)
	var ps []message.Position
	for i := range 6 {
		ps = append(ps, appendBody(t, h.Store, ns, fmt.Sprintf("m%d", i)))
	}
	for _, p := range ps[1:5] {
		require.NoError(t, h.Store.Delete(ctx, ns, p))
	}
	assert.Equal(t, []message.Position{ps[0], ps[5]}, positionsIn(scanPass(t, h.Store, 0, 1, nil), ns), "a deleted run is skipped, not a stop")

	require.NoError(t, h.Store.Delete(ctx, ns, ps[5]))
	assert.Equal(t, []message.Position{ps[0]}, positionsIn(scanPass(t, h.Store, 0, 1, nil), ns), "a deleted tail ends the pass")
}

func testScanBound(t *testing.T, h Harness) {
	ns := newNamespace(t, 0)
	before := []message.Position{appendBody(t, h.Store, ns, "a"), appendBody(t, h.Store, ns, "b"), appendBody(t, h.Store, ns, "c")}
	var after []message.Position
	// The second page holds the bound and has room for one more, so a store
	// that reads past its bound is caught.
	all := scanPass(t, h.Store, 0, 2, func() {
		after = append(after, appendBody(t, h.Store, ns, "d"), appendBody(t, h.Store, ns, "e"))
	})
	got := positionsIn(all, ns)
	assert.Equal(t, before, got, "appends after the bound are not in the pass")
	assert.Equal(t, append(before, after...), positionsIn(scanPass(t, h.Store, 0, 3, nil), ns), "a new pass has a new bound")
}

func testScanBadInput(t *testing.T, h Harness) {
	ctx := context.Background()
	for _, limit := range []int{0, -1} {
		_, err := h.Store.Scan(ctx, 0, message.ScanCursor{}, limit)
		assert.ErrorIsf(t, err, message.ErrInvalidArgument, "limit %d", limit)
	}
	_, err := h.Store.Scan(ctx, 0, message.ScanCursor{After: 5, Bound: 4}, 1)
	assert.ErrorIs(t, err, message.ErrInvalidArgument, "a cursor past its bound")
}
