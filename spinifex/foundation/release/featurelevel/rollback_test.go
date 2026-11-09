package featurelevel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func atLevelOne(t *testing.T) *Store {
	t.Helper()
	s, _ := newStore(t)
	_, adv, err := s.Advance(t.Context(), members(1, true, true))
	require.NoError(t, err)
	require.True(t, adv)
	return s
}

// ackFor answers the way a healthy node does once it has read the rollback.
func ackFor(s *Store, node string, inFlight map[int]int) NodeStatus {
	_, rev, _ := s.Read(context.Background())
	total := 0
	for _, c := range inFlight {
		total += c
	}
	return NodeStatus{Node: node, Reply: &StatusReply{Node: node, Build: Build, Revision: rev, InFlight: inFlight, InFlightTotal: total}}
}

func deps(s *Store, probe func(context.Context) []NodeStatus, drains func(int) map[int]DrainCheck) RollbackDeps {
	if drains == nil {
		drains = func(int) map[int]DrainCheck { return map[int]DrainCheck{1: NothingToDrain} }
	}
	return RollbackDeps{Store: s, Probe: probe, Drains: drains, Build: Build, AckTimeout: 150 * time.Millisecond, PollInterval: 10 * time.Millisecond}
}

func TestRollback_FullSuccessLowersTheLevelAndKeepsTheHold(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	probe := func(context.Context) []NodeStatus { return []NodeStatus{ackFor(s, "a", nil), ackFor(s, "b", nil)} }
	rec, err := RunRollback(t.Context(), 0, deps(s, probe, nil))
	require.NoError(t, err)
	assert.Zero(t, rec.Level)
	assert.Nil(t, rec.Rollback)
	assert.Equal(t, Hold{Held: true, Level: 0}, rec.Hold)
	got, _, _ := s.Read(t.Context())
	assert.Equal(t, rec, got)
}

func TestRollback_SilentNodeFailsAndLeavesTheRollbackInPlace(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	probe := func(context.Context) []NodeStatus {
		return []NodeStatus{ackFor(s, "a", nil), {Node: "b", Reason: "no-reply"}}
	}
	_, err := RunRollback(t.Context(), 0, deps(s, probe, nil))
	var unacked *UnackedError
	require.ErrorAs(t, err, &unacked)
	assert.Equal(t, []string{"b"}, unacked.Nodes)
	assert.Contains(t, err.Error(), "b (no usable answer: no-reply)")
	got, _, _ := s.Read(t.Context())
	assert.Equal(t, 1, got.Level, "the level is not lowered")
	require.NotNil(t, got.Rollback)
	assert.Equal(t, 0, got.Rollback.Target)
}

func TestRollback_AStaleRevisionIsNotAnAck(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	probe := func(context.Context) []NodeStatus {
		st := ackFor(s, "a", nil)
		st.Reply.Revision--
		return []NodeStatus{st}
	}
	_, err := RunRollback(t.Context(), 0, deps(s, probe, nil))
	var unacked *UnackedError
	require.ErrorAs(t, err, &unacked)
	assert.Contains(t, unacked.Reasons["a"], "has read revision")
}

func TestRollback_InFlightAboveTargetBlocksUntilItIsDone(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	var calls atomic.Int32
	probe := func(context.Context) []NodeStatus {
		if calls.Add(1) < 3 {
			return []NodeStatus{ackFor(s, "a", map[int]int{1: 2})}
		}
		return []NodeStatus{ackFor(s, "a", nil)}
	}
	d := deps(s, probe, nil)
	d.AckTimeout = 2 * time.Second
	rec, err := RunRollback(t.Context(), 0, d)
	require.NoError(t, err)
	assert.Zero(t, rec.Level)
	assert.GreaterOrEqual(t, calls.Load(), int32(3), "it polled until the in-flight work was gone")

	s2 := atLevelOne(t)
	stuck := func(context.Context) []NodeStatus { return []NodeStatus{ackFor(s2, "a", map[int]int{1: 1})} }
	_, err = RunRollback(t.Context(), 0, deps(s2, stuck, nil))
	var unacked *UnackedError
	require.ErrorAs(t, err, &unacked)
	assert.Contains(t, unacked.Reasons["a"], "1 in flight above level 0")
}

func TestRollback_InFlightAtOrBelowTheTargetDoesNotBlock(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	probe := func(context.Context) []NodeStatus { return []NodeStatus{ackFor(s, "a", map[int]int{0: 4})} }
	_, err := RunRollback(t.Context(), 0, deps(s, probe, nil))
	require.NoError(t, err)
}

func TestRollback_OutstandingDrainBlocksAndLeavesTheRollback(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	probe := func(context.Context) []NodeStatus { return []NodeStatus{ackFor(s, "a", nil)} }
	left := 3
	drains := func(int) map[int]DrainCheck {
		return map[int]DrainCheck{1: func(context.Context) (int, error) { return left, nil }}
	}
	_, err := RunRollback(t.Context(), 0, deps(s, probe, drains))
	var out *DrainOutstandingError
	require.ErrorAs(t, err, &out)
	assert.Equal(t, 1, out.Level)
	assert.Equal(t, 3, out.Outstanding)
	got, _, _ := s.Read(t.Context())
	assert.Equal(t, 1, got.Level)
	assert.NotNil(t, got.Rollback)

	left = 0
	rec, err := RunRollback(t.Context(), 0, deps(s, probe, drains))
	require.NoError(t, err, "the same command resumes once the work is gone")
	assert.Zero(t, rec.Level)
}

func TestRollback_ADrainErrorAndAMissingCheckBothBlock(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	probe := func(context.Context) []NodeStatus { return []NodeStatus{ackFor(s, "a", nil)} }
	boom := errors.New("kv down")
	_, err := RunRollback(t.Context(), 0, deps(s, probe, func(int) map[int]DrainCheck {
		return map[int]DrainCheck{1: func(context.Context) (int, error) { return 0, boom }}
	}))
	require.ErrorIs(t, err, boom)

	_, err = RunRollback(t.Context(), 0, deps(s, probe, func(int) map[int]DrainCheck { return nil }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drain check is registered")
	got, _, _ := s.Read(t.Context())
	assert.Equal(t, 1, got.Level)
}

func TestRollback_ResumesWithTheSameTargetAfterAFailure(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	silent := func(context.Context) []NodeStatus { return []NodeStatus{{Node: "a", Reason: "no-reply"}} }
	_, err := RunRollback(t.Context(), 0, deps(s, silent, nil))
	require.Error(t, err)
	healthy := func(context.Context) []NodeStatus { return []NodeStatus{ackFor(s, "a", nil)} }
	rec, err := RunRollback(t.Context(), 0, deps(s, healthy, nil))
	require.NoError(t, err)
	assert.Zero(t, rec.Level)
}

func TestRollback_OldCLIRefusesBeforeWriting(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	_, before, _ := s.Read(t.Context())
	d := deps(s, func(context.Context) []NodeStatus { return nil }, nil)
	d.Build = 0
	_, err := RunRollback(t.Context(), 0, d)
	require.ErrorIs(t, err, ErrCLITooOld)
	got, after, _ := s.Read(t.Context())
	assert.Equal(t, before, after)
	assert.Nil(t, got.Rollback)
	assert.False(t, got.Hold.Held)
}

func TestRollback_NoMembersCannotAcknowledge(t *testing.T) {
	t.Parallel()
	s := atLevelOne(t)
	_, err := RunRollback(t.Context(), 0, deps(s, func(context.Context) []NodeStatus { return nil }, nil))
	require.Error(t, err)
	got, _, _ := s.Read(t.Context())
	assert.NotNil(t, got.Rollback)
}

func TestServeStatus_NilGateAnswersWithAnErrorNeverZeroInFlight(t *testing.T) {
	t.Parallel()
	var g *Gate
	_, err := g.ServeStatus("n1", Build)(t.Context(), &StatusRequest{Node: "n1"}, "")
	require.Error(t, err)
}
