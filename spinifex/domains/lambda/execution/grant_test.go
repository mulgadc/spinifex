package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	s, err := Open(context.Background(), testutil.NewJetStream(t, nc))
	require.NoError(t, err)
	c := &clock{t: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)}
	s.now = c.now
	return s, c
}

func attempt(c *clock, id string) Attempt {
	return Attempt{
		Grant:    Grant{AttemptID: id, EnvironmentID: "env-" + id, Generation: 1, Node: "node1"},
		Identity: identity(id), LaunchBy: c.now().Add(time.Minute),
	}
}

func identity(id string) Identity {
	return Identity{RequestID: "req-" + id, AccountID: "123456789012", Region: "ap-southeast-2",
		FunctionID: "fn1", RevisionID: "rev1", CodeSHA256: "aa"}
}

func state(t *testing.T, s *Store, id string) string {
	t.Helper()
	rec, _, err := s.read(context.Background(), "node1")
	require.NoError(t, err)
	require.NotNil(t, rec.Attempt)
	require.Equal(t, id, rec.Attempt.Grant.AttemptID)
	return rec.Attempt.State
}

func started(t *testing.T, s *Store, c *clock, id string) Attempt {
	t.Helper()
	a, err := s.Issue(context.Background(), attempt(c, id))
	require.NoError(t, err)
	require.NoError(t, s.Start(context.Background(), a.Grant, a.Identity))
	return a
}

func TestGrant_LifecycleFreesTheSlotOnlyOnProvenStop(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a := started(t, s, c, "a1")
	require.Equal(t, uint64(1), a.Grant.Seq)
	require.NotEmpty(t, a.Grant.GrantID)

	_, err := s.Issue(ctx, attempt(c, "a2"))
	require.ErrorIs(t, err, ErrNodeBusy, "no second grant while the first runs")

	require.NoError(t, s.Accept(ctx, a.Grant))
	require.ErrorIs(t, s.Accept(ctx, a.Grant), ErrWrongState, "one result per attempt")
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.ErrorIs(t, err, ErrNodeBusy, "an accepted result does not free the slot")

	require.NoError(t, s.Released(ctx, a.Grant))
	require.NoError(t, s.Stopped(ctx, a.Grant))
	require.Equal(t, StateEnded, state(t, s, "a1"))
	require.ErrorIs(t, s.Accept(ctx, a.Grant), ErrWrongState, "no result accepted after the end")

	b, err := s.Issue(ctx, attempt(c, "a2"))
	require.NoError(t, err)
	require.Equal(t, uint64(2), b.Grant.Seq, "later grants order after earlier ones")
}

func TestGrant_AmbiguousHoldsTheSlotUntilStopIsProven(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a := started(t, s, c, "a1")
	require.NoError(t, s.Ambiguous(ctx, a.Grant))
	require.NoError(t, s.Ambiguous(ctx, a.Grant), "holding again is a no-op")

	c.add(24 * time.Hour)
	_, err := s.Issue(ctx, attempt(c, "a2"))
	require.ErrorIs(t, err, ErrNodeBusy, "time alone never releases an ambiguous attempt")
	require.ErrorIs(t, s.Accept(ctx, a.Grant), ErrWrongState, "no result accepted while ambiguous")
	require.ErrorIs(t, s.Withdraw(ctx, a.Grant), ErrWrongState, "a started attempt cannot be withdrawn")

	require.NoError(t, s.Released(ctx, a.Grant))
	require.NoError(t, s.Stopped(ctx, a.Grant))
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.NoError(t, err)
}

func TestGrant_StartRefusesReplayAndEveryMismatchedIdentity(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)

	mismatched := map[string]func(g *Grant){
		"grant ID":    func(g *Grant) { g.GrantID = "other" },
		"older seq":   func(g *Grant) { g.Seq-- },
		"environment": func(g *Grant) { g.EnvironmentID = "env-other" },
		"generation":  func(g *Grant) { g.Generation++ },
		"attempt":     func(g *Grant) { g.AttemptID = "a9" },
		"node":        func(g *Grant) { g.Node = "node2" },
	}
	for name, mutate := range mismatched {
		g := a.Grant
		mutate(&g)
		require.ErrorIs(t, s.Start(ctx, g, a.Identity), ErrNotCurrent, name)
		require.ErrorIs(t, s.Stopped(ctx, g), ErrNotCurrent, name)
	}

	require.NoError(t, s.Start(ctx, a.Grant, a.Identity))
	require.ErrorIs(t, s.Start(ctx, a.Grant, a.Identity), ErrWrongState, "a replayed start is refused")
}

func TestGrant_ExpiredGrantIsSupersededOnlyIfItNeverStarted(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	old, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)

	c.add(2 * time.Minute)
	require.ErrorIs(t, s.Start(ctx, old.Grant, old.Identity), ErrExpired)

	next, err := s.Issue(ctx, attempt(c, "a2"))
	require.NoError(t, err, "a holder that can never start does not block the node")
	require.Greater(t, next.Grant.Seq, old.Grant.Seq)
	require.ErrorIs(t, s.Start(ctx, old.Grant, old.Identity), ErrNotCurrent, "the superseded grant stays refused")

	// A started holder is never superseded, however late.
	require.NoError(t, s.Start(ctx, next.Grant, next.Identity))
	c.add(24 * time.Hour)
	_, err = s.Issue(ctx, attempt(c, "a3"))
	require.ErrorIs(t, err, ErrNodeBusy)
	require.Equal(t, StateRunning, state(t, s, "a2"))
}

func TestGrant_WithdrawFreesOnlyANeverStartedGrant(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)
	require.NoError(t, s.Withdraw(ctx, a.Grant))
	require.ErrorIs(t, s.Start(ctx, a.Grant, a.Identity), ErrWrongState, "a withdrawn grant can never start")
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.NoError(t, err)
}

func TestGrant_UnexpiredGrantedHolderBlocks(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	_, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.ErrorIs(t, err, ErrNodeBusy)
}

func TestGrant_ConcurrentIssueGrantsExactlyOne(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	// Seed the slot so racers contend on the conditional update, not Create.
	seed, err := s.Issue(ctx, attempt(c, "seed"))
	require.NoError(t, err)
	require.NoError(t, s.Withdraw(ctx, seed.Grant))

	const racers = 16
	var wg sync.WaitGroup
	results := make(chan error, racers)
	for i := range racers {
		wg.Go(func() {
			_, err := s.Issue(ctx, attempt(c, fmt.Sprintf("r%d", i)))
			results <- err
		})
	}
	wg.Wait()
	close(results)
	granted := 0
	for err := range results {
		if err == nil {
			granted++
		} else {
			require.ErrorIs(t, err, ErrNodeBusy)
		}
	}
	require.Equal(t, 1, granted)
}

// TestGrant_StartLosesToASupersedingGrant forces the interleaving where a
// Start has read an expiring grant and a new grant replaces it before Start
// writes: Start must fail, leaving only the new grant able to run.
func TestGrant_StartLosesToASupersedingGrant(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	old, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)

	// Start reads at a time the old grant is still valid; the new grant is
	// issued after expiry, between Start's read and its write.
	var next Attempt
	s.beforeWrite = func() {
		s.beforeWrite = nil
		c.add(2 * time.Minute)
		next, err = s.Issue(ctx, attempt(c, "a2"))
		require.NoError(t, err)
	}
	require.ErrorIs(t, s.Start(ctx, old.Grant, old.Identity), ErrNotCurrent)
	c.add(-2 * time.Minute)
	require.NoError(t, s.Start(ctx, next.Grant, next.Identity))
	require.ErrorIs(t, s.Start(ctx, old.Grant, old.Identity), ErrNotCurrent)
	require.Equal(t, StateRunning, state(t, s, "a2"))
}

func TestGrant_AcceptLosesToAConcurrentHold(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a := started(t, s, c, "a1")
	s.beforeWrite = func() {
		s.beforeWrite = nil
		require.NoError(t, s.Ambiguous(ctx, a.Grant))
	}
	require.ErrorIs(t, s.Accept(ctx, a.Grant), ErrNotCurrent, "the result was not accepted")
	require.Equal(t, StateAmbiguous, state(t, s, "a1"))
}

func TestGrant_DuplicateAttemptIsRefused(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a := started(t, s, c, "a1")
	require.NoError(t, s.Released(ctx, a.Grant))
	require.NoError(t, s.Stopped(ctx, a.Grant))

	_, err := s.Issue(ctx, attempt(c, "a1"))
	require.ErrorIs(t, err, ErrWrongState, "an attempt ID is not granted twice in a row")
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.NoError(t, err, "the refused duplicate does not take the slot")
}

// lostAck lands attempt Creates and then reports a failure, as a lost
// acknowledgement would.
type lostAck struct{ jetstream.KeyValue }

func (l lostAck) Create(ctx context.Context, key string, v []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if _, err := l.KeyValue.Create(ctx, key, v, opts...); err != nil {
		return 0, err
	}
	return 0, errors.New("ack lost")
}

func TestGrant_AmbiguousRecordWriteKeepsTheSlot(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	inner := s.kv
	s.kv = lostAck{inner}
	_, err := s.Issue(ctx, attempt(c, "a1"))
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	s.kv = inner

	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.ErrorIs(t, err, ErrNodeBusy, "a grant that may be recorded keeps the slot")
	require.Equal(t, StateGranted, state(t, s, "a1"))
	c.add(2 * time.Minute)
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.NoError(t, err, "past its deadline the landed grant can never start")
}

func TestGrant_UnrecognisedRecordIsRefusedUnchanged(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)
	require.NoError(t, s.Withdraw(ctx, a.Grant))

	e, err := s.kv.Get(ctx, nodeKey("node1"))
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(e.Value(), &raw))
	raw["formatVersion"] = formatVersion + 1
	body, _ := json.Marshal(raw)
	rev, err := s.kv.Update(ctx, nodeKey("node1"), body, e.Revision())
	require.NoError(t, err)

	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.ErrorIs(t, err, ErrUnknownRecord)
	require.ErrorIs(t, s.Start(ctx, a.Grant, a.Identity), ErrUnknownRecord)
	after, err := s.kv.Get(ctx, nodeKey("node1"))
	require.NoError(t, err)
	require.Equal(t, rev, after.Revision())
	require.Equal(t, body, after.Value())
}

func TestGrant_StartRefusesAMismatchedRequestUnchanged(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)
	mismatched := map[string]func(id *Identity){
		"request":  func(id *Identity) { id.RequestID = "req-other" },
		"account":  func(id *Identity) { id.AccountID = "210987654321" },
		"region":   func(id *Identity) { id.Region = "us-east-1" },
		"function": func(id *Identity) { id.FunctionID = "fn2" },
		"revision": func(id *Identity) { id.RevisionID = "rev2" },
		"code":     func(id *Identity) { id.CodeSHA256 = "bb" },
	}
	for name, mutate := range mismatched {
		want := a.Identity
		mutate(&want)
		require.ErrorIs(t, s.Start(ctx, a.Grant, want), ErrMismatch, name)
		require.Equal(t, StateGranted, state(t, s, "a1"), name)
	}
	require.NoError(t, s.Start(ctx, a.Grant, a.Identity))
}

// A released attempt still holds the node; only Stopped, and only after
// Released, frees it for the next grant.
func TestReleasedHoldsTheNodeUntilStopped(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	a, err := s.Issue(ctx, attempt(c, "a1"))
	require.NoError(t, err)
	require.NoError(t, s.Start(ctx, a.Grant, a.Identity))
	require.ErrorIs(t, s.Stopped(ctx, a.Grant), ErrWrongState)

	require.NoError(t, s.Released(ctx, a.Grant))
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.ErrorIs(t, err, ErrNodeBusy)

	require.NoError(t, s.Stopped(ctx, a.Grant))
	_, err = s.Issue(ctx, attempt(c, "a2"))
	require.NoError(t, err)
}

// The launch identity is recorded once, complete, and only while running, so
// recovery never acts on a partial or replaced identity.
func TestGrant_LaunchedRecordsACompleteIdentityOnce(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	l := Launch{MachineID: "m", BootID: "b", PIDNamespace: 4026531836, LauncherPID: 10,
		LauncherInode: 11, PID: 20, Inode: 21}

	granted, err := s.Issue(ctx, attempt(c, "a0"))
	require.NoError(t, err)
	require.ErrorIs(t, s.Launched(ctx, granted.Grant, l), ErrWrongState, "not yet running")
	require.NoError(t, s.Withdraw(ctx, granted.Grant))

	a := started(t, s, c, "a1")
	partial := l
	partial.Inode = 0
	require.Error(t, s.Launched(ctx, a.Grant, partial))
	require.NoError(t, s.Launched(ctx, a.Grant, l))
	other := l
	other.PID = 99
	require.ErrorIs(t, s.Launched(ctx, a.Grant, other), ErrWrongState, "recorded once")

	rec, err := s.Current(ctx, "node1")
	require.NoError(t, err)
	require.NotNil(t, rec.Launch)
	require.Equal(t, l, *rec.Launch)
}
