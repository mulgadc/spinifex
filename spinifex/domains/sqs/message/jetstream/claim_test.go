package jetstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var claimNS = message.Namespace{MessageShard: 1, StateShard: 1, Queue: "claimq"}

const claimRetention = 4 * 24 * time.Hour

// appendAt stores a message enqueued at enq and visible from vis.
func appendAt(t *testing.T, s message.Store, enq, vis time.Time) message.Position {
	t.Helper()
	res, err := s.Append(context.Background(), claimNS, message.AppendInput{Body: []byte("hello"), Meta: map[string]string{
		message.MetaV: "1", message.MetaID: "id-" + strconv.FormatInt(enq.UnixNano(), 10),
		message.MetaEnq: strconv.FormatInt(enq.UnixMilli(), 10), message.MetaVis: strconv.FormatInt(vis.UnixMilli(), 10),
		message.MetaSender: sender, message.MetaMD5: "5d41402abc4b2a76b9719d911017c592",
	}})
	require.NoError(t, err)
	return res.Position
}

// claimStore is a single-node dedicated store, as the first profile runs.
func claimStore(t *testing.T) message.Store {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	return openStore(t, storeMaxPayload)
}

func claimAt(s message.Store, now time.Time) *message.Claimer {
	return &message.Claimer{Store: s, Now: func() time.Time { return now }}
}

func claimPos(pos message.Position, vt time.Duration) message.ClaimInput {
	return message.ClaimInput{Namespace: claimNS, Position: pos, Visibility: vt, Retention: claimRetention}
}

func TestClaimLifecycleOnJetStream(t *testing.T) {
	s := claimStore(t)
	ctx := context.Background()
	pos := appendAt(t, s, fixedNow, fixedNow.Add(10*time.Second))

	_, err := claimAt(s, fixedNow).Claim(ctx, claimPos(pos, 30*time.Second))
	require.ErrorIs(t, err, message.ErrNotVisible, "delayed")
	_, err = s.GetState(ctx, claimNS, pos)
	require.ErrorIs(t, err, message.ErrNotFound, "a delayed message has no state record")

	t1 := fixedNow.Add(10 * time.Second)
	first, err := claimAt(s, t1).Claim(ctx, claimPos(pos, 30*time.Second))
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.Generation)
	assert.Equal(t, []byte("hello"), first.Message.Body)

	_, err = claimAt(s, t1.Add(29*time.Second)).Claim(ctx, claimPos(pos, 30*time.Second))
	require.ErrorIs(t, err, message.ErrNotVisible, "in flight")

	t2 := t1.Add(30 * time.Second)
	second, err := claimAt(s, t2).Claim(ctx, claimPos(pos, 5*time.Second))
	require.NoError(t, err, "visible again at the deadline")
	assert.Equal(t, uint64(2), second.ReceiveCount)
	assert.Equal(t, uint64(2), second.Generation)
	assert.Equal(t, t1.UnixMilli(), second.FirstReceive.UnixMilli())
	assert.Equal(t, t2.Add(5*time.Second).UnixMilli(), second.Deadline.UnixMilli())

	st, err := s.GetState(ctx, claimNS, pos)
	require.NoError(t, err)
	assert.Equal(t, second.Revision, st.Revision, "the claim reports the acknowledged revision")
}

// Competing receivers on one message: exactly one claim is acknowledged.
func TestClaimCompetingReceiversOnJetStream(t *testing.T) {
	s := claimStore(t)
	ctx := context.Background()
	for _, prior := range []bool{false, true} {
		pos := appendAt(t, s, fixedNow, fixedNow)
		now := fixedNow.Add(time.Minute)
		if prior {
			_, err := claimAt(s, fixedNow.Add(time.Second)).Claim(ctx, claimPos(pos, time.Second))
			require.NoError(t, err, "an expired earlier claim makes the race a revision race")
		}
		const receivers = 16
		var wg sync.WaitGroup
		results := make([]error, receivers)
		claims := make([]message.Claim, receivers)
		for i := range receivers {
			wg.Go(func() {
				claims[i], results[i] = claimAt(s, now).Claim(ctx, claimPos(pos, time.Minute))
			})
		}
		wg.Wait()
		won := 0
		for i, err := range results {
			switch {
			case err == nil:
				won++
				assert.Equal(t, uint64(1+boolInt(prior)), claims[i].Generation)
			case errors.Is(err, message.ErrClaimLost), errors.Is(err, message.ErrNotVisible):
			default:
				t.Fatalf("receiver %d: %v", i, err)
			}
		}
		assert.Equal(t, 1, won, "prior claim %v", prior)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// lostClaimReply applies the claim, then reports a lost acknowledgement.
type lostClaimReply struct {
	message.Store

	mu    sync.Mutex
	calls int
}

func (l *lostClaimReply) PutState(ctx context.Context, ns message.Namespace, pos message.Position, v []byte, exp message.Revision) (message.Revision, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	if _, err := l.Store.PutState(ctx, ns, pos, v, exp); err != nil {
		return 0, err
	}
	return 0, errors.New("nats: timeout")
}

func TestClaimAppliedButLostReplyOnJetStream(t *testing.T) {
	s := claimStore(t)
	ctx := context.Background()
	pos := appendAt(t, s, fixedNow, fixedNow)
	lossy := &lostClaimReply{Store: s}

	_, err := claimAt(lossy, fixedNow).Claim(ctx, claimPos(pos, 30*time.Second))
	require.ErrorIs(t, err, message.ErrUnavailable)
	assert.Equal(t, 1, lossy.calls, "the claim is not resent")

	st, err := s.GetState(ctx, claimNS, pos)
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(st.Value, &rec))
	assert.Equal(t, 1.0, rec["c"], "the unanswered claim still counts as a receive")

	_, err = claimAt(s, fixedNow.Add(29*time.Second)).Claim(ctx, claimPos(pos, 30*time.Second))
	require.ErrorIs(t, err, message.ErrNotVisible, "no other receiver gets it before its deadline")
	again, err := claimAt(s, fixedNow.Add(30*time.Second)).Claim(ctx, claimPos(pos, 30*time.Second))
	require.NoError(t, err)
	assert.Equal(t, uint64(2), again.ReceiveCount)
	assert.Equal(t, uint64(2), again.Generation, "the lost generation is never reissued")
}

// hookedStore runs a hook after the n-th body read, and before each state
// read and after each state write, so a test can interleave other writers or
// move the clock at exact points of one claim.
type hookedStore struct {
	message.Store

	afterGet     func(n int)
	beforeState  func()
	beforePut    func()
	afterPut     func()
	gets, states int
}

func (h *hookedStore) Get(ctx context.Context, ns message.Namespace, pos message.Position) (message.Message, error) {
	m, err := h.Store.Get(ctx, ns, pos)
	h.gets++
	if h.afterGet != nil {
		h.afterGet(h.gets)
	}
	return m, err
}

func (h *hookedStore) GetState(ctx context.Context, ns message.Namespace, pos message.Position) (message.State, error) {
	h.states++
	if h.beforeState != nil {
		h.beforeState()
	}
	return h.Store.GetState(ctx, ns, pos)
}

func (h *hookedStore) PutState(ctx context.Context, ns message.Namespace, pos message.Position, v []byte, rev message.Revision) (message.Revision, error) {
	if h.beforePut != nil {
		h.beforePut()
	}
	r, err := h.Store.PutState(ctx, ns, pos, v, rev)
	if h.afterPut != nil {
		h.afterPut()
	}
	return r, err
}

// A deletion that tombstones the record, deletes the body and removes the
// tombstone between the claim's body read and its state read leaves the
// record absent again; the claim must not resurrect the deleted message.
func TestClaimDoesNotResurrectRemovedMessage(t *testing.T) {
	s := claimStore(t)
	ctx := context.Background()
	pos := appendAt(t, s, fixedNow, fixedNow)
	held, err := claimAt(s, fixedNow).Claim(ctx, claimPos(pos, time.Minute))
	require.NoError(t, err)

	removed := false
	h := &hookedStore{Store: s, afterGet: func(n int) {
		if n != 1 {
			return
		}
		tomb, err := json.Marshal(map[string]any{"v": 1, "s": message.StatusDeleted})
		require.NoError(t, err)
		tombRev, err := s.PutState(ctx, claimNS, pos, tomb, held.Revision)
		require.NoError(t, err, "DeleteMessage tombstones the record")
		require.NoError(t, s.Delete(ctx, claimNS, pos), "the sweeper deletes the body")
		require.NoError(t, s.DeleteState(ctx, claimNS, pos, tombRev), "then the tombstone")
		removed = true
	}}

	later := fixedNow.Add(2 * time.Minute)
	got, err := claimAt(h, later).Claim(ctx, claimPos(pos, time.Minute))
	require.True(t, removed, "the deletion ran inside the claim")
	require.ErrorIs(t, err, message.ErrGone)
	assert.Nil(t, got.Message.Body, "no deleted body is returned")
	_, err = s.GetState(ctx, claimNS, pos)
	assert.ErrorIs(t, err, message.ErrNotFound, "the record the claim created is withdrawn")
}

// steppingClock is a test clock a hook can move forward.
type steppingClock struct{ at time.Time }

func (c *steppingClock) now() time.Time {
	return c.at
}

func (c *steppingClock) step(d time.Duration) {
	c.at = c.at.Add(d)
}

func (c *steppingClock) claimer(s message.Store) *message.Claimer {
	return &message.Claimer{Store: s, Now: c.now}
}

// A slow body or state read does not spend the visibility: the deadline is
// measured from the claim attempt, after both reads.
func TestClaimDeadlineFromAttemptNotRequestStart(t *testing.T) {
	s := claimStore(t)
	ctx := context.Background()
	pos := appendAt(t, s, fixedNow, fixedNow)
	clock := &steppingClock{at: fixedNow}
	h := &hookedStore{Store: s,
		afterGet: func(n int) {
			if n == 1 {
				clock.step(40 * time.Second)
			}
		},
		beforeState: func() { clock.step(40 * time.Second) },
	}

	got, err := clock.claimer(h).Claim(ctx, claimPos(pos, 30*time.Second))
	require.NoError(t, err)
	attempt := fixedNow.Add(80 * time.Second)
	assert.Equal(t, attempt.UnixMilli(), got.ReceivedAt.UnixMilli())
	assert.Equal(t, attempt.Add(30*time.Second).UnixMilli(), got.Deadline.UnixMilli())
	assert.Equal(t, attempt.UnixMilli(), got.FirstReceive.UnixMilli())
}
