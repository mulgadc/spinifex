package ochre

//test:in-package: drives the unexported health-hold writes and the reaper and
// eviction admission paths that must refuse a held endpoint.

import (
	stderrors "errors"
	"testing"
	"time"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errHoldAckLost = stderrors.New("hold ack lost")

func testHold() HealthHold {
	return HealthHold{ReservedAt: time.Now().UTC().Truncate(time.Second)}
}

func readRecord(t *testing.T, s *Service, key string) (EndpointRecord, uint64) {
	t.Helper()
	rec, rev, found, err := s.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	return rec, rev
}

// reserveObserved reserves a hold on the record as it is now stored.
func reserveObserved(t *testing.T, s *Service, key string) uint64 {
	t.Helper()
	rec, rev := readRecord(t, s, key)
	_, next, err := s.reserveHealthHold(t.Context(), key, rec, rev, testHold())
	require.NoError(t, err)
	return next
}

func TestHealthHold_ReserveAndClearAtTheObservedRevision(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t, newLaunchHarness(), 200, sufficientGPU())
	seeded := seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
	key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)

	observed, rev := readRecord(t, s, key)
	hold := HealthHold{ReservedAt: time.Now().UTC().Truncate(time.Second)}
	_, heldRev, err := s.reserveHealthHold(t.Context(), key, observed, rev, hold)
	require.NoError(t, err)

	held, storedRev := readRecord(t, s, key)
	assert.Equal(t, heldRev, storedRev)
	require.NotNil(t, held.HealthHold)
	assert.Equal(t, hold, *held.HealthHold, "the reservation time must round-trip")
	want := seeded
	want.HealthHold = &hold
	want.Generation = seeded.Generation + 1
	assert.Equal(t, want, held, "the hold must change nothing else")

	_, clearedRev, err := s.clearHealthHold(t.Context(), key, held, heldRev, nil)
	require.NoError(t, err)
	cleared, storedRev := readRecord(t, s, key)
	assert.Equal(t, clearedRev, storedRev)
	want.HealthHold = nil
	want.Generation = seeded.Generation + 2
	assert.Equal(t, want, cleared)
}

// A hold or a clear is fenced on the revision it observed: any later write,
// even one that leaves the hold itself alone, refuses it.
func TestHealthHold_ChangedRecordConflicts(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t, newLaunchHarness(), 200, sufficientGPU())
	seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
	key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)

	stale, staleRev := readRecord(t, s, key)
	rewriteVictim(t, s, func(rec *EndpointRecord) { rec.LastActiveAt = time.Now().UTC() })
	moved, _ := readRecord(t, s, key)

	_, _, err := s.reserveHealthHold(t.Context(), key, stale, staleRev, testHold())
	require.ErrorIs(t, err, kvstore.ErrConflict)
	after, _ := readRecord(t, s, key)
	assert.Equal(t, moved, after, "a conflicting reserve must write nothing")

	heldRev := reserveObserved(t, s, key)
	held, _ := readRecord(t, s, key)
	rewriteVictim(t, s, func(rec *EndpointRecord) { rec.InFlight = 1 })
	changed, _ := readRecord(t, s, key)

	_, _, err = s.clearHealthHold(t.Context(), key, held, heldRev, nil)
	require.ErrorIs(t, err, kvstore.ErrConflict)
	after, _ = readRecord(t, s, key)
	assert.Equal(t, changed, after, "a clear from an older revision must leave the hold in place")
	assert.True(t, after.Held())
}

func TestHealthHold_RefusesTheWrongStartingState(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t, newLaunchHarness(), 200, sufficientGPU())
	seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
	key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)

	ready, readyRev := readRecord(t, s, key)
	_, _, err := s.clearHealthHold(t.Context(), key, ready, readyRev, nil)
	require.ErrorIs(t, err, errHoldAbsent)

	rewriteVictim(t, s, func(rec *EndpointRecord) { rec.State = StateStarting; rec.Generation++ })
	starting, startingRev := readRecord(t, s, key)
	_, _, err = s.reserveHealthHold(t.Context(), key, starting, startingRev, testHold())
	require.ErrorIs(t, err, errHoldNotReady)

	rewriteVictim(t, s, func(rec *EndpointRecord) { rec.State = StateReady; rec.Generation++ })
	reserveObserved(t, s, key)
	held, heldRev := readRecord(t, s, key)
	_, _, err = s.reserveHealthHold(t.Context(), key, held, heldRev, testHold())
	require.ErrorIs(t, err, errHoldExists)

	after, afterRev := readRecord(t, s, key)
	assert.Equal(t, held, after)
	assert.Equal(t, heldRev, afterRev, "a refused hold or clear must not write")
}

// An error other than a conflict leaves the outcome unknown and is returned
// once, unretried. A later read shows only what was visible then.
func TestHealthHold_UncertainWriteIsNotRetried(t *testing.T) {
	t.Parallel()
	for name, landed := range map[string]bool{"landed, ack lost": true, "never landed": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestService(t, newLaunchHarness(), 200, sufficientGPU())
			seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
			key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)

			calls := 0
			s.holdWrite = func(write func() (uint64, error)) (uint64, error) {
				calls++
				if landed {
					_, err := write()
					require.NoError(t, err)
				}
				return 0, errHoldAckLost
			}
			observed, rev := readRecord(t, s, key)
			_, _, err := s.reserveHealthHold(t.Context(), key, observed, rev, testHold())
			require.ErrorIs(t, err, errHoldAckLost)
			assert.NotErrorIs(t, err, kvstore.ErrConflict)
			assert.Equal(t, 1, calls, "an uncertain hold must not be retried")

			s.holdWrite = nil
			after, afterRev := readRecord(t, s, key)
			assert.Equal(t, landed, after.Held())
			if landed {
				// The writer never saw the new revision, so it cannot clear from it.
				_, _, err = s.clearHealthHold(t.Context(), key, after, rev, nil)
				require.ErrorIs(t, err, kvstore.ErrConflict)
				_, _, err = s.clearHealthHold(t.Context(), key, after, afterRev, nil)
				require.NoError(t, err)
			} else {
				assert.Equal(t, rev, afterRev)
			}
		})
	}
}

// A held record that has left READY, such as one an explicit delete has moved
// to DRAINING, is never rewritten by a clear.
func TestHealthHold_ClearRefusesAHeldRecordPastReady(t *testing.T) {
	t.Parallel()
	for _, state := range []EndpointState{StateDraining, StateStarting} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			s, _ := newTestService(t, newLaunchHarness(), 200, sufficientGPU())
			seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
			key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)
			reserveObserved(t, s, key)
			rewriteVictim(t, s, func(rec *EndpointRecord) { rec.State = state; rec.Generation++ })

			calls := 0
			s.holdWrite = func(write func() (uint64, error)) (uint64, error) {
				calls++
				return write()
			}
			held, heldRev := readRecord(t, s, key)
			require.True(t, held.Held())
			_, _, err := s.clearHealthHold(t.Context(), key, held, heldRev, nil)
			require.ErrorIs(t, err, errHoldNotReady)
			assert.Zero(t, calls, "no write may be attempted")

			after, afterRev := readRecord(t, s, key)
			assert.Equal(t, held, after)
			assert.Equal(t, heldRev, afterRev, "the owner's revision must survive")
		})
	}
}

// A held endpoint whose probe fails stays held and idle; the next direct
// healthy probe clears the hold, and only then does idle reclaim apply.
func TestReaper_LaterHealthyProbeClearsTheHold(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	rec := f.ready(t)
	aged := f.age(t, 10*time.Minute, 10*time.Minute)
	f.transport.breakScrapes()

	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	held, found := f.current(t)
	require.True(t, found)
	require.True(t, held.Held(), "a failed probe must leave the hold it reserved")
	assert.Equal(t, StateReady, held.State)
	assert.True(t, held.LastActiveAt.Equal(aged.LastActiveAt), "a failed probe is not activity")
	assert.Zero(t, held.ScrapeFailures)
	assert.Empty(t, f.harness.launcher.terminations())

	f.transport.serve(0, 0, 0)
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	_, found = f.current(t)
	assert.False(t, found, "once a healthy probe clears the hold, the idle endpoint is reclaimed as before")
	assert.Contains(t, f.harness.launcher.terminations(), rec.InstanceID)
	assert.EqualValues(t, 1, f.harness.launcher.launchCount.Load(), "nothing is relaunched")
}

// A reclaim that observed the record before a hold landed must not tear it
// down, whether the hold lands before admission or between admission and the
// draining CAS.
func TestService_ReclaimRefusesAHoldReservedAfterObservation(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{"before admission": false, "after admission": true}
	for name, duringAdmit := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReaperFixture(t, ReaperDeps{})
			observed := f.ready(t)
			key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)

			stillLeads := func() bool { return true }
			if duringAdmit {
				stillLeads = func() bool {
					reserveObserved(t, f.svc, key)
					return true
				}
			} else {
				reserveObserved(t, f.svc, key)
			}
			require.ErrorIs(t, f.svc.reclaim(t.Context(), observed, stillLeads), errReclaimSuperseded)

			cur, found := f.current(t)
			require.True(t, found)
			assert.Equal(t, StateReady, cur.State)
			assert.True(t, cur.Held())
			assert.Empty(t, f.harness.launcher.terminations())
		})
	}
}

func TestEvictForCapacity_HeldEndpointIsNeverSelected(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, exhaustedGPU())
	seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
	reserveObserved(t, s, EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel))

	selected := 0
	s.afterEvictionSelect = func(EndpointRecord) { selected++ }
	assert.False(t, s.evictForCapacity(t.Context(), testModelID, 5120))
	assert.Zero(t, selected)
	assert.Empty(t, h.launcher.terminations())
}

func TestEvictForCapacity_HoldAfterSelectionIsNotTornDown(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, exhaustedGPU())
	seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
	key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)

	var selected []EndpointRecord
	s.afterEvictionSelect = func(victim EndpointRecord) {
		selected = append(selected, victim)
		reserveObserved(t, s, key)
	}
	assert.False(t, s.evictForCapacity(t.Context(), testModelID, 5120))
	require.Len(t, selected, 1)
	assert.False(t, selected[0].Held(), "the victim must have been chosen before the hold")
	assert.Empty(t, h.launcher.terminations())

	cur, _ := readRecord(t, s, key)
	assert.Equal(t, StateReady, cur.State)
	assert.True(t, cur.Held())
}

// A customer's delete is not automatic and is not held back.
func TestDelete_HeldEndpointIsStillDeleted(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, sufficientGPU())
	seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
	key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)
	reserveObserved(t, s, key)

	out, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: evictionVictimModel}, "")
	require.NoError(t, err)
	assert.True(t, out.Removed)
	assert.Equal(t, []string{"i-other"}, h.launcher.terminations())
	_, _, found, err := s.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	assert.False(t, found)
}
