package ochre

//test:in-package: drives the reaper's held probe through the unexported
// health-hold write seam and the capacity-eviction selection hook.

import (
	"net/http"
	"testing"
	"time"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdWriteAt fails the nth health-hold write (1-based) as fail directs and
// passes every other write through. It returns the count of writes seen.
func holdWriteAt(f *reaperFixture, n int, fail func(write func() (uint64, error)) (uint64, error)) *int {
	calls := new(int)
	f.svc.holdWrite = func(write func() (uint64, error)) (uint64, error) {
		*calls++
		if *calls == n {
			return fail(write)
		}
		return write()
	}
	return calls
}

// competingWrite moves the stored record on before write runs, so the write
// is fenced on a revision that is no longer current.
func competingWrite(t *testing.T, f *reaperFixture, write func() (uint64, error)) (uint64, error) {
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	rec, rev, found, err := f.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	rec.InFlight = 7
	_, err = f.store.CompareAndSet(t.Context(), key, &rec, rev)
	require.NoError(t, err)
	return write()
}

func idleFixture(t *testing.T) (*reaperFixture, EndpointRecord) {
	t.Helper()
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	rec := f.ready(t)
	f.age(t, 10*time.Minute, 10*time.Minute)
	return f, rec
}

func assertUntouched(t *testing.T, f *reaperFixture, instanceID string) EndpointRecord {
	t.Helper()
	f.svc.WaitLaunches()
	cur, found := f.current(t)
	require.True(t, found)
	assert.Equal(t, StateReady, cur.State)
	assert.Equal(t, instanceID, cur.InstanceID)
	assert.Empty(t, f.harness.launcher.terminations())
	assert.EqualValues(t, 1, f.harness.launcher.launchCount.Load())
	return cur
}

// A hold that cannot be reserved from the sweep's read leaves the endpoint
// unprobed and untouched, even one idle long enough to reclaim.
func TestReaper_HoldReserveConflictOrUncertaintyYields(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		fail     func(t *testing.T, f *reaperFixture, write func() (uint64, error)) (uint64, error)
		wantHeld bool
	}{
		"conflict": {fail: competingWrite},
		"landed, ack lost": {fail: func(t *testing.T, _ *reaperFixture, write func() (uint64, error)) (uint64, error) {
			_, err := write()
			require.NoError(t, err)
			return 0, errHoldAckLost
		}, wantHeld: true},
		"never landed": {fail: func(*testing.T, *reaperFixture, func() (uint64, error)) (uint64, error) {
			return 0, errHoldAckLost
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := idleFixture(t)
			calls := holdWriteAt(f, 1, func(write func() (uint64, error)) (uint64, error) { return tc.fail(t, f, write) })

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			cur := assertUntouched(t, f, rec.InstanceID)
			assert.Equal(t, 1, *calls, "the reserve must not be retried")
			assert.Zero(t, f.transport.scrapeCount(), "an endpoint that could not be held is not probed")
			assert.Equal(t, tc.wantHeld, cur.Held())
		})
	}
}

// A healthy probe whose clear conflicts or is uncertain neither reclaims nor
// retries; a conflicting writer's record survives with the hold still set.
func TestReaper_HoldClearConflictOrUncertaintyYields(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		fail     func(t *testing.T, f *reaperFixture, write func() (uint64, error)) (uint64, error)
		wantHeld bool
	}{
		"conflict": {fail: competingWrite, wantHeld: true},
		"landed, ack lost": {fail: func(t *testing.T, _ *reaperFixture, write func() (uint64, error)) (uint64, error) {
			_, err := write()
			require.NoError(t, err)
			return 0, errHoldAckLost
		}},
		"never landed": {fail: func(*testing.T, *reaperFixture, func() (uint64, error)) (uint64, error) {
			return 0, errHoldAckLost
		}, wantHeld: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := idleFixture(t)
			calls := holdWriteAt(f, 2, func(write func() (uint64, error)) (uint64, error) { return tc.fail(t, f, write) })

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			cur := assertUntouched(t, f, rec.InstanceID)
			assert.Equal(t, 2, *calls, "the clear must not be retried")
			assert.Equal(t, 1, f.transport.scrapeCount())
			assert.Equal(t, tc.wantHeld, cur.Held())
			if name == "conflict" {
				assert.Equal(t, 7, cur.InFlight, "the competing writer's record must survive")
			}
		})
	}
}

// A hold left by an earlier sweep is observed from the same read, not
// reserved again, and cleared only from that read's revision.
func TestReaper_ExistingHoldIsObservedAndClearedFromItsRead(t *testing.T) {
	t.Parallel()
	f, rec := idleFixture(t)
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	observed, rev := readRecord(t, f.svc, key)
	_, heldRev, err := f.svc.reserveHealthHold(t.Context(), key, observed, rev, testHold())
	require.NoError(t, err)

	var revs []uint64
	f.svc.holdWrite = func(write func() (uint64, error)) (uint64, error) {
		_, rev := readRecord(t, f.svc, key)
		revs = append(revs, rev)
		return write()
	}
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

	assert.Equal(t, []uint64{heldRev}, revs, "one write, the clear, from the held revision")
	_, found := f.current(t)
	assert.False(t, found, "the cleared idle endpoint is reclaimed")
	assert.Contains(t, f.harness.launcher.terminations(), rec.InstanceID)
}

// A capacity eviction that selected its victim before the reaper's sweep held
// it must not tear it down, whether the probe then fails or succeeds.
func TestEvictForCapacity_ReaperHoldAfterSelectionIsNotTornDown(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*vllmTransport){
		"failed probe":  func(v *vllmTransport) { v.breakScrapes() },
		"healthy probe": func(v *vllmTransport) { v.serve(0, 0, 0) },
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusOK, exhaustedGPU())
			transport := &vllmTransport{}
			answer(transport)
			s.deps.HTTPClient = &http.Client{Transport: transport}
			seeded := seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))
			reaper := testReaper(t, s, testNodeID, ReaperDeps{IdleTTL: 24 * time.Hour})

			selected := 0
			s.afterEvictionSelect = func(victim EndpointRecord) {
				selected++
				require.False(t, victim.Held())
				require.NoError(t, reaper.sweepOnce(t.Context(), nil))
			}
			assert.False(t, s.evictForCapacity(t.Context(), testModelID, 5120))
			assert.Equal(t, 1, selected)
			assert.Equal(t, 1, transport.scrapeCount(), "the reaper must have probed between selection and teardown")
			assert.Empty(t, h.launcher.terminations())

			cur, _ := readRecord(t, s, EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel))
			assert.Equal(t, StateReady, cur.State)
			assert.Equal(t, seeded.InstanceID, cur.InstanceID)
			assert.Equal(t, name == "failed probe", cur.Held())
		})
	}
}

// A tenant record stored under a global key is not the endpoint that key
// addresses: it is neither held, probed nor reaped, though idle past its TTL.
func TestReaper_TenantRecordUnderAGlobalKeyIsRefused(t *testing.T) {
	t.Parallel()
	f, rec := idleFixture(t)
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	stored, rev := readRecord(t, f.svc, key)
	stored.AccountID = testAccountID
	rev, err := f.store.CompareAndSet(t.Context(), key, &stored, rev)
	require.NoError(t, err)
	writes := holdWriteAt(f, 0, nil)

	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

	got, gotRev := readRecord(t, f.svc, key)
	assert.Equal(t, stored, got)
	assert.Equal(t, rev, gotRev, "no write may land on a mismatched record")
	assert.Zero(t, *writes)
	assert.Zero(t, f.transport.scrapeCount())
	assert.Empty(t, f.harness.launcher.terminations(), rec.InstanceID)
	assert.EqualValues(t, 1, f.harness.launcher.launchCount.Load())
}
