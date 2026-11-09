package featurelevel

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) (*Store, jetstream.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "cluster-config-test", History: 1})
	require.NoError(t, err)
	return NewStore(kv), kv
}

func members(level int, current ...bool) []Member {
	var out []Member
	for i, c := range current {
		out = append(out, Member{Node: string(rune('a' + i)), Level: level, Current: c})
	}
	return out
}

func TestAbsentRecordReadsAsLevelZeroAndGarbageIsAnError(t *testing.T) {
	t.Parallel()
	s, kv := newStore(t)
	rec, rev, err := s.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Record{}, rec)
	assert.Zero(t, rev)

	_, err = kv.Put(t.Context(), Key, []byte("{not json"))
	require.NoError(t, err)
	_, _, err = s.Read(t.Context())
	require.Error(t, err)
}

func TestAdvance_OnlyWhenEveryMemberIsCurrentAndNeverLowers(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	rec, advanced, err := s.Advance(t.Context(), members(1, true, false, true))
	require.NoError(t, err)
	assert.False(t, advanced, "a stale member blocks advance")
	assert.Zero(t, rec.Level)

	_, advanced, err = s.Advance(t.Context(), []Member{{Node: "a", Level: 1, Current: true}, {Node: "b", Level: 0, Current: true}})
	require.NoError(t, err)
	assert.False(t, advanced, "a member reporting an absent field (0) blocks advance")

	_, advanced, err = s.Advance(t.Context(), nil)
	require.NoError(t, err)
	assert.False(t, advanced, "an unknown member set never advances")

	rec, advanced, err = s.Advance(t.Context(), members(1, true, true, true))
	require.NoError(t, err)
	assert.True(t, advanced)
	assert.Equal(t, 1, rec.Level)

	rec, advanced, err = s.Advance(t.Context(), members(1, true, false, true))
	require.NoError(t, err)
	assert.False(t, advanced)
	assert.Equal(t, 1, rec.Level, "a member going stale afterwards does not lower the level")

	rec, _, err = s.Advance(t.Context(), members(0, true, true))
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Level, "members reporting lower never lower it")
}

func TestAdvance_IsCappedAtThisBuild(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	rec, _, err := s.Advance(t.Context(), members(Build+5, true, true))
	require.NoError(t, err)
	assert.Equal(t, Build, rec.Level, "a daemon cannot advance past a level it does not understand")
}

func TestAdvance_ConcurrentDaemonsLeaveOneConsistentRecord(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	var wg sync.WaitGroup
	var advanced atomic.Int32
	for range 8 {
		wg.Go(func() {
			_, ok, err := s.Advance(t.Context(), members(1, true, true, true))
			assert.NoError(t, err)
			if ok {
				advanced.Add(1)
			}
		})
	}
	wg.Wait()
	assert.EqualValues(t, 1, advanced.Load(), "the compare-and-set makes exactly one writer win")
	rec, _, err := s.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Record{Level: 1}, rec)
}

func TestHold_CapsAdvance(t *testing.T) {
	t.Parallel()
	s, kv := newStore(t)
	_, err := kv.Create(t.Context(), Key, []byte(`{"level":0,"hold":{"held":true,"level":0}}`))
	require.NoError(t, err)
	rec, advanced, err := s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	assert.False(t, advanced)
	assert.Zero(t, rec.Level, "a hold at 0 is a hold, not no hold")

	_, err = s.ReleaseHold(t.Context())
	require.NoError(t, err)
	rec, advanced, err = s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	assert.True(t, advanced)
	assert.Equal(t, 1, rec.Level)
}

func TestRollbackToZeroHoldsAgainstRepeatedTicks(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	_, _, err := s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)

	rec, rev, err := s.StartRollback(t.Context(), 0, Build)
	require.NoError(t, err)
	assert.Equal(t, &Rollback{Target: 0}, rec.Rollback)
	assert.Equal(t, Hold{Held: true, Level: 0}, rec.Hold)
	assert.NotZero(t, rev)

	_, advanced, err := s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	assert.False(t, advanced, "advance stops while the rollback is in progress")

	rec, err = s.FinishRollback(t.Context())
	require.NoError(t, err)
	assert.Zero(t, rec.Level)
	assert.Nil(t, rec.Rollback)
	assert.Equal(t, Hold{Held: true, Level: 0}, rec.Hold, "the hold outlives the rollback")

	for range 5 {
		rec, advanced, err = s.Advance(t.Context(), members(1, true, true, true))
		require.NoError(t, err)
		assert.False(t, advanced)
		assert.Zero(t, rec.Level, "running newer builds do not re-advance a level rolled back to 0")
	}

	_, err = s.ReleaseHold(t.Context())
	require.NoError(t, err)
	rec, advanced, err = s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	assert.True(t, advanced)
	assert.Equal(t, 1, rec.Level)
}

func TestRollback_StateTransitions(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	_, _, err := s.StartRollback(t.Context(), 0, Build)
	require.ErrorIs(t, err, ErrBadTarget, "nothing above level 0 to roll back")
	_, err = s.FinishRollback(t.Context())
	require.ErrorIs(t, err, ErrNoRollback)
	_, err = s.CancelRollback(t.Context())
	require.ErrorIs(t, err, ErrNoRollback)

	_, _, err = s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	_, _, err = s.StartRollback(t.Context(), 1, Build)
	require.ErrorIs(t, err, ErrBadTarget)
	_, _, err = s.StartRollback(t.Context(), -1, Build)
	require.ErrorIs(t, err, ErrBadTarget)

	first, rev1, err := s.StartRollback(t.Context(), 0, Build)
	require.NoError(t, err)
	again, rev2, err := s.StartRollback(t.Context(), 0, Build)
	require.NoError(t, err)
	assert.Equal(t, first, again, "repeating the same rollback resumes it")
	assert.Equal(t, rev1, rev2, "and writes nothing")

	_, err = s.ReleaseHold(t.Context())
	require.ErrorIs(t, err, ErrRollbackInProgress)

	rec, err := s.CancelRollback(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Record{Level: 1}, rec, "cancel clears the rollback and the hold it set")
	_, advanced, err := s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	assert.False(t, advanced, "already at level 1")
}

func TestStartRollback_CLIBelowRecordedLevelRefusesWithoutWriting(t *testing.T) {
	t.Parallel()
	s, kv := newStore(t)
	_, err := kv.Create(t.Context(), Key, []byte(`{"level":2,"hold":{"held":false,"level":0}}`))
	require.NoError(t, err)
	_, before, err := s.Read(t.Context())
	require.NoError(t, err)

	_, _, err = s.StartRollback(t.Context(), 0, 1)
	require.ErrorIs(t, err, ErrCLITooOld)
	rec, after, err := s.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, before, after, "no write")
	assert.Nil(t, rec.Rollback)
}

func TestAdmit_FalseAboveARollbackTargetFromTheMomentItIsWritten(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	g := NewGate(s)

	done, ok, err := g.Admit(t.Context(), 1)
	require.NoError(t, err)
	done()
	assert.False(t, ok, "level 0 does not admit level 1")

	_, _, err = s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	done, ok, err = g.Admit(t.Context(), 1)
	require.NoError(t, err)
	done()
	assert.True(t, ok)

	_, _, err = s.StartRollback(t.Context(), 0, Build)
	require.NoError(t, err)
	done, ok, err = g.Admit(t.Context(), 1)
	require.NoError(t, err)
	done()
	assert.False(t, ok, "false for N above the target the moment the rollback is written")
	done, ok, err = g.Admit(t.Context(), 0)
	require.NoError(t, err)
	done()
	assert.True(t, ok, "levels at or below the target stay active")
}

// gatedKV blocks Get until released, to pause a request between counting it in
// flight and reading the record.
type gatedKV struct {
	KV

	entered chan struct{}
	release chan struct{}
	fail    bool
}

func (k *gatedKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.entered != nil {
		close(k.entered)
		<-k.release
	}
	if k.fail {
		return nil, context.DeadlineExceeded
	}
	return k.KV.Get(ctx, key)
}

func TestAdmit_CountsInFlightBeforeReadingAndUntilDone(t *testing.T) {
	t.Parallel()
	s, kv := newStore(t)
	_, _, err := s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	paused := &gatedKV{KV: kv, entered: make(chan struct{}), release: make(chan struct{})}
	g := NewGate(NewStore(paused))

	type result struct {
		done func()
		ok   bool
	}
	res := make(chan result, 1)
	go func() {
		done, ok, err := g.Admit(t.Context(), 1)
		assert.NoError(t, err)
		res <- result{done, ok}
	}()
	<-paused.entered
	assert.Equal(t, 1, g.InFlightAbove(0), "counted before the record is read")

	close(paused.release)
	r := <-res
	assert.True(t, r.ok)
	assert.Equal(t, 1, g.InFlightAbove(0), "still counted after admission until the caller finishes recording")
	assert.Zero(t, g.InFlightAbove(1), "only levels above the target count")
	r.done()
	r.done()
	assert.Zero(t, g.InFlightAbove(0), "done is idempotent and releases the count")
	assert.Empty(t, g.InFlight())
}

func TestAdmit_ReadFailureIsAnErrorAndStillReleasesTheCount(t *testing.T) {
	t.Parallel()
	s, kv := newStore(t)
	_ = s
	g := NewGate(NewStore(&gatedKV{KV: kv, fail: true}))
	done, ok, err := g.Admit(t.Context(), 1)
	require.Error(t, err)
	assert.False(t, ok)
	assert.Equal(t, 1, g.InFlightAbove(0))
	done()
	assert.Zero(t, g.InFlightAbove(0))
}

func TestEveryDeclaredLevelHasExactlyOneDrainCheck(t *testing.T) {
	t.Parallel()
	assert.Empty(t, UncoveredLevels(), "every declared level must register a drain check")
	require.NotEmpty(t, Levels())
	assert.Equal(t, Build, Levels()[len(Levels())-1].N, "Build is the highest declared level")

	assert.Equal(t, []int{2}, uncovered([]Level{{N: 1}, {N: 2}}, map[int]DrainCheck{1: NothingToDrain}), "a level with no check is reported")
	require.Error(t, RegisterDrain(Baseline, NothingToDrain), "a second check for a level is refused")
	require.Error(t, RegisterDrain(Build+10, NothingToDrain), "an undeclared level is refused")
	require.Error(t, RegisterDrain(Baseline, nil))
	assert.Contains(t, DrainChecks(0), Baseline)
	assert.NotContains(t, DrainChecks(Baseline), Baseline, "only levels above the target drain")
}

func TestCancelRollback_RestoresThePriorHold(t *testing.T) {
	t.Parallel()
	s, kv := newStore(t)
	// Held at 1 after an earlier rollback that was never released.
	_, err := kv.Create(t.Context(), Key, []byte(`{"level":1,"hold":{"held":true,"level":1}}`))
	require.NoError(t, err)

	rec, _, err := s.StartRollback(t.Context(), 0, Build)
	require.NoError(t, err)
	assert.Equal(t, Hold{Held: true, Level: 0}, rec.Hold)
	assert.Equal(t, Hold{Held: true, Level: 1}, rec.Rollback.PriorHold)

	rec, err = s.CancelRollback(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Hold{Held: true, Level: 1}, rec.Hold, "the hold that existed before the rollback is back, not cleared")
	assert.Nil(t, rec.Rollback)
	assert.Equal(t, 1, rec.Level)

	// A cluster that had no hold goes back to no hold.
	s2, _ := newStore(t)
	_, _, err = s2.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	_, _, err = s2.StartRollback(t.Context(), 0, Build)
	require.NoError(t, err)
	rec, err = s2.CancelRollback(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Hold{}, rec.Hold)
}
