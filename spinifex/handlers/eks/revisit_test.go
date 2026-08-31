package handlers_eks

//test:in-package — asserts on the deadline reconcileOnce returns and on when a
// state report wakes the loop, both of which are unexported and are the whole
// subject of these tests.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// woke reports whether storeReport signalled, draining the channel so a later
// call in the same test starts from quiet.
func woke(r *ClusterReconciler) bool {
	select {
	case <-r.wake:
		return true
	default:
		return false
	}
}

// A CREATING cluster's artifacts are keys in the watched bucket and its first
// healthy report is a trigger, so the timeout expiring is the only thing left
// that announces itself to nobody.
func TestReconcileOnce_ACreatingClusterAsksForWhatIsLeftOfItsCreateTimeout(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t, WithCreateTimeout(15*time.Minute))
	freshenClusterCreatedAt(t, acctKV)
	// No bootstrap state and no report, so the cluster stays CREATING.

	revisit, err := r.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Greater(t, revisit, 14*time.Minute, "a cluster created seconds ago has nearly all of its window left")
	assert.LessOrEqual(t, revisit, 15*time.Minute)
}

// A create timeout already past belongs to the pass that fails the cluster.
// Returning nothing would leave that pass unscheduled.
func TestReconcileOnce_ACreatingClusterPastItsTimeoutAsksForTheInterval(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t,
		WithCreateTimeout(15*time.Minute),
		WithReconcileInterval(30*time.Second),
	)
	meta, err := GetClusterMeta(t.Context(), acctKV, "alpha")
	require.NoError(t, err)
	meta.CreatedAt = time.Now().UTC().Add(-time.Hour)
	require.NoError(t, PutClusterMeta(t.Context(), acctKV, meta))

	// The pass marks it FAILED, which is terminal and reported as such.
	revisit, err := r.reconcileOnce(t.Context())
	require.ErrorIs(t, err, ErrReconcilerClusterFailed)
	assert.Equal(t, 30*time.Second, revisit)
}

// A healthy cluster is waiting for its control plane to go silent, and silence
// is published by nobody. The deadline is the instant the newest report expires,
// which lands the pass on it rather than up to a tick after it.
func TestReconcileOnce_AHealthyClusterAsksForItsReportsStalenessInstant(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t, WithStateStaleAfter(90*time.Second))
	require.NoError(t, SetClusterStatus(t.Context(), acctKV, "alpha", ClusterStatusActive))
	r.latest.Store(freshReport("ok", 3))

	revisit, err := r.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Greater(t, revisit, 60*time.Second, "a report seconds old must not read as nearly stale")
	assert.LessOrEqual(t, revisit, 90*time.Second)
}

// A report already past its window is the case the next pass has to notice, so
// the deadline falls back to the interval rather than to nothing.
func TestReconcileOnce_AClusterWithAStaleReportAsksForTheInterval(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t,
		WithStateStaleAfter(90*time.Second),
		WithReconcileInterval(30*time.Second),
	)
	require.NoError(t, SetClusterStatus(t.Context(), acctKV, "alpha", ClusterStatusActive))
	r.latest.Store(&ServerStateReport{Healthz: "ok", NodeCount: 2, TS: time.Now().Add(-5 * time.Minute).Unix()})

	revisit, err := r.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 30*time.Second, revisit)
}

// A degraded cluster keeps the old cadence: the recovery ladders run on elapsed
// time that this deliberately does not restate, and they poll their members'
// VM state on every pass regardless.
func TestReconcileOnce_ADegradedClusterAsksForTheInterval(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t, WithReconcileInterval(30*time.Second))
	require.NoError(t, SetClusterStatus(t.Context(), acctKV, "alpha", ClusterStatusActive))
	r.latest.Store(freshReport("fail", 0))

	revisit, err := r.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 30*time.Second, revisit)
}

// Without a state source health comes from an HTTP probe, which answers only
// when asked, so there is nothing to wait for and the poll has to stay.
func TestReconcileOnce_AProbedClusterAsksForTheInterval(t *testing.T) {
	stub := &stubHTTPDoer{status: 200}
	r, _, acctKV := newReconcilerHarness(t, "https://nlb.example/healthz",
		WithHTTPClient(stub),
		WithReconcileInterval(30*time.Second),
	)
	require.NoError(t, SetClusterStatus(t.Context(), acctKV, "alpha", ClusterStatusActive))

	revisit, err := r.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 30*time.Second, revisit)
}

// A terminal status ends the loop, so there is no next run to schedule.
func TestReconcileOnce_ADeletingClusterAsksForNoDeadline(t *testing.T) {
	r, _, acctKV := newStateReconcilerHarness(t)
	require.NoError(t, SetClusterStatus(t.Context(), acctKV, "alpha", ClusterStatusDeleting))

	revisit, err := r.reconcileOnce(t.Context())
	require.ErrorIs(t, err, ErrReconcilerClusterDeleting)
	assert.Zero(t, revisit)
}

// The first report is always news: there is nothing to compare it against, and
// it is what flips a CREATING cluster.
func TestStoreReport_TheFirstReportWakesTheLoop(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t)

	r.storeReport(freshReport("ok", 3))

	assert.True(t, woke(r), "the first report must wake the loop")
}

// The control plane publishes on its own timer whether or not anything changed.
// Waking on every report would be the old 30s tick under another name.
func TestStoreReport_ARepeatedReportDoesNotWakeTheLoop(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t)
	r.storeReport(&ServerStateReport{Healthz: "ok", NodeCount: 3, TS: time.Now().Unix()})
	require.True(t, woke(r))

	r.storeReport(&ServerStateReport{Healthz: "ok", NodeCount: 3, TS: time.Now().Add(30 * time.Second).Unix()})

	assert.False(t, woke(r), "a report saying the same thing later must not wake the loop")
}

// The newest report still has to be stored even when it does not wake anything,
// because the staleness deadline is measured from it.
func TestStoreReport_ARepeatedReportStillRefreshesWhatIsStored(t *testing.T) {
	r, _, _ := newStateReconcilerHarness(t)
	r.storeReport(&ServerStateReport{Healthz: "ok", NodeCount: 3, TS: 1000})

	r.storeReport(&ServerStateReport{Healthz: "ok", NodeCount: 3, TS: 2000})

	assert.Equal(t, int64(2000), r.latest.Load().TS)
}

func TestStoreReport_AChangedReportWakesTheLoop(t *testing.T) {
	for _, tc := range []struct {
		name string
		next *ServerStateReport
	}{
		{"healthz", &ServerStateReport{Healthz: "fail", NodeCount: 3}},
		{"reason", &ServerStateReport{Healthz: "ok", NodeCount: 3, Reason: "etcd:unreachable"}},
		{"node count", &ServerStateReport{Healthz: "ok", NodeCount: 2}},
		{"nodegroup counts", &ServerStateReport{Healthz: "ok", NodeCount: 3, NodegroupReady: map[string]int{"ng": 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := newStateReconcilerHarness(t)
			r.storeReport(&ServerStateReport{Healthz: "ok", NodeCount: 3})
			require.True(t, woke(r))

			r.storeReport(tc.next)

			assert.True(t, woke(r), "a report that changes health must wake the loop")
		})
	}
}
