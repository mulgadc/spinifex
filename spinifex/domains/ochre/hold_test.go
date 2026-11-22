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
	next, err := s.reserveHealthHold(t.Context(), key, rec, rev, testHold())
	require.NoError(t, err)
	return next
}

func TestHealthHold_ReserveAndClearAtTheObservedRevision(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t, newLaunchHarness(), 200, sufficientGPU())
	seeded := seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
	key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)

	observed, rev := readRecord(t, s, key)
	hold := HealthHold{Token: "sweep-1", Epoch: 7, ReservedAt: time.Now().UTC().Truncate(time.Second)}
	heldRev, err := s.reserveHealthHold(t.Context(), key, observed, rev, hold)
	require.NoError(t, err)

	held, storedRev := readRecord(t, s, key)
	assert.Equal(t, heldRev, storedRev)
	require.NotNil(t, held.HealthHold)
	assert.Equal(t, hold, *held.HealthHold, "token and epoch must round-trip for the sweep journal")
	want := seeded
	want.HealthHold = &hold
	want.Generation = seeded.Generation + 1
	assert.Equal(t, want, held, "the hold must change nothing else")

	clearedRev, err := s.clearHealthHold(t.Context(), key, held, heldRev)
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

	_, err := s.reserveHealthHold(t.Context(), key, stale, staleRev, testHold())
	require.ErrorIs(t, err, kvstore.ErrConflict)
	after, _ := readRecord(t, s, key)
	assert.Equal(t, moved, after, "a conflicting reserve must write nothing")

	heldRev := reserveObserved(t, s, key)
	held, _ := readRecord(t, s, key)
	rewriteVictim(t, s, func(rec *EndpointRecord) { rec.InFlight = 1 })
	changed, _ := readRecord(t, s, key)

	_, err = s.clearHealthHold(t.Context(), key, held, heldRev)
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
	_, err := s.clearHealthHold(t.Context(), key, ready, readyRev)
	require.ErrorIs(t, err, errHoldAbsent)

	rewriteVictim(t, s, func(rec *EndpointRecord) { rec.State = StateStarting; rec.Generation++ })
	starting, startingRev := readRecord(t, s, key)
	_, err = s.reserveHealthHold(t.Context(), key, starting, startingRev, testHold())
	require.ErrorIs(t, err, errHoldNotReady)

	rewriteVictim(t, s, func(rec *EndpointRecord) { rec.State = StateReady; rec.Generation++ })
	reserveObserved(t, s, key)
	held, heldRev := readRecord(t, s, key)
	_, err = s.reserveHealthHold(t.Context(), key, held, heldRev, testHold())
	require.ErrorIs(t, err, errHoldExists)

	after, afterRev := readRecord(t, s, key)
	assert.Equal(t, held, after)
	assert.Equal(t, heldRev, afterRev, "a refused hold or clear must not write")
}

// An error other than a conflict leaves the outcome unknown. It is returned
// once, unretried, and only a fresh read can tell whether the write landed.
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
			_, err := s.reserveHealthHold(t.Context(), key, observed, rev, testHold())
			require.ErrorIs(t, err, errHoldAckLost)
			assert.NotErrorIs(t, err, kvstore.ErrConflict)
			assert.Equal(t, 1, calls, "an uncertain hold must not be retried")

			s.holdWrite = nil
			after, afterRev := readRecord(t, s, key)
			assert.Equal(t, landed, after.Held())
			if landed {
				// The writer never saw the new revision, so it cannot clear from it.
				_, err = s.clearHealthHold(t.Context(), key, after, rev)
				require.ErrorIs(t, err, kvstore.ErrConflict)
				_, err = s.clearHealthHold(t.Context(), key, after, afterRev)
				require.NoError(t, err)
			} else {
				assert.Equal(t, rev, afterRev)
			}
		})
	}
}

func TestReaper_NeverReapsHeldEndpoint(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	rec := f.ready(t)
	f.age(t, 10*time.Minute, 10*time.Minute)
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	heldRev := reserveObserved(t, f.svc, key)

	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	cur, found := f.current(t)
	require.True(t, found)
	assert.Equal(t, StateReady, cur.State)
	assert.True(t, cur.Held())
	assert.Empty(t, f.harness.launcher.terminations())

	held, _ := readRecord(t, f.svc, key)
	_, err := f.svc.clearHealthHold(t.Context(), key, held, heldRev)
	require.NoError(t, err)
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	_, found = f.current(t)
	assert.False(t, found, "once cleared, the idle endpoint is reclaimed as before")
	assert.Contains(t, f.harness.launcher.terminations(), rec.InstanceID)
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
