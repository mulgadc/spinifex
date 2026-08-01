package handlers_eks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markDeleting transitions a seeded cluster to DELETING and backdates
// DeletingSince so the reaper treats it as wedged (past min-age).
func markDeleting(t *testing.T, f *deleteClusterFixture, name string, age time.Duration) {
	t.Helper()
	require.NoError(t, SetClusterStatus(t.Context(), f.kv, name, ClusterStatusDeleting))
	meta, err := GetClusterMeta(t.Context(), f.kv, name)
	require.NoError(t, err)
	meta.DeletingSince = time.Now().UTC().Add(-age)
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
}

// TestRLC4_DeletingReaperReDrivesWedgedTeardown locks the contract: a cluster
// stuck in DELETING past min-age (its synchronous DeleteCluster failed and no
// client re-issued) must be re-driven to completion by the backstop reaper —
// infra torn down and meta swept — so its billable EIP is never stranded.
func TestRLC4_DeletingReaperReDrivesWedgedTeardown(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	markDeleting(t, f, "alpha", 10*time.Minute)

	reaper := f.svc.NewDeletingReaper()
	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the wedged DELETING cluster must be re-driven")

	_, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound, "meta must be swept after the backstop completes teardown")
	assert.GreaterOrEqual(t, len(f.inst.terminateCalls), 1, "CP VM must be terminated")
	assert.Len(t, f.eip.releaseCalls, 1, "the billable egress EIP must be released, not stranded")
}

// TestDeletingReaperSkipsFreshDelete: a cluster that just entered DELETING is
// within the in-flight synchronous-delete window; the reaper must not race it.
func TestDeletingReaperSkipsFreshDelete(t *testing.T) {
	f := newDeleteClusterFixture(t, "beta")
	markDeleting(t, f, "beta", 1*time.Second) // younger than min-age

	reaper := f.svc.NewDeletingReaper()
	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a freshly-DELETING cluster must be left to its in-flight delete")

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "beta")
	require.NoError(t, getErr, "meta must survive — the reaper did not re-drive")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.Empty(t, f.inst.terminateCalls, "no teardown must run within the min-age window")
}

// TestDeletingReaper_EnumerationFailureIsReported: the KV bucket lister closes
// its channel the same way whether the listing completed or failed, so a sweep
// that ignored the terminal error would report "nothing wedged" for an
// unreachable JetStream — leaving a stuck teardown holding its billable EIP with
// nothing to say the sweep never looked.
func TestDeletingReaper_EnumerationFailureIsReported(t *testing.T) {
	f := newDeleteClusterFixture(t, "delta")
	markDeleting(t, f, "delta", 10*time.Minute)

	// Closing the connection fails the stream-names request behind the listing.
	f.svc.deps.NATSConn.Close()

	reaper := f.svc.NewDeletingReaper()
	n, err := reaper.Sweep(context.Background())
	require.Error(t, err, "a failed bucket enumeration must not report a completed sweep")
	assert.Zero(t, n)
}

// TestDeletingReaper_BacksOffAfterFailedAttempt guards against the re-drive
// re-running on every 2-minute GC tick with no backoff: a permanently-failing
// purge must not be retried again until the exponential backoff window (2×
// minAge after 1 prior attempt) has elapsed, and the attempt is persisted so
// the window survives a restart. No sleeping — the clock is advanced by
// backdating LastDeleteReapAttempt directly.
func TestDeletingReaper_BacksOffAfterFailedAttempt(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")
	markDeleting(t, f, "alpha", 10*time.Minute)

	reaper := f.svc.NewDeletingReaper()

	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err, "Sweep logs and continues past a single cluster's re-drive failure")
	assert.Equal(t, 0, n)
	assert.Len(t, f.inst.terminateCalls, 1, "the first re-drive attempt must run")

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, 1, meta.DeleteReapAttempts, "the attempt must be counted")
	assert.False(t, meta.LastDeleteReapAttempt.IsZero(), "the attempt time must be stamped")

	// Immediately re-sweeping must be a no-op: still inside the backoff window.
	n, err = reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Len(t, f.inst.terminateCalls, 1, "must not re-drive again inside the backoff window")

	// Advance past the backoff window without sleeping, by backdating the
	// last-attempt timestamp; the reaper must then re-drive again.
	meta.LastDeleteReapAttempt = time.Now().UTC().Add(-2 * deleteReapBackoff(deletingReapMinAge, meta.DeleteReapAttempts))
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))

	n, err = reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Len(t, f.inst.terminateCalls, 2, "must re-drive again once the backoff window elapses")

	meta, getErr = GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, 2, meta.DeleteReapAttempts)
}

// TestDeletingReaper_ExhaustsAfterMaxAttempts guards the terminal give-up: a
// purge that keeps failing the same way forever (e.g. an unretriable
// DependencyViolation) must stop being re-driven after maxDeleteReapAttempts,
// not loop forever on every GC tick. It also locks the ADR-0006 §6 billing
// invariant: an exhausted cluster must stay DELETING — its infra stays tracked
// and billable — not silently vanish or move to some other status.
func TestDeletingReaper_ExhaustsAfterMaxAttempts(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("permanent hypervisor failure")
	markDeleting(t, f, "alpha", 10*time.Minute)

	reaper := f.svc.NewDeletingReaper()

	for i := 1; i <= maxDeleteReapAttempts; i++ {
		n, err := reaper.Sweep(context.Background())
		require.NoError(t, err, "Sweep logs and continues past a single cluster's re-drive failure")
		assert.Equal(t, 0, n)

		meta, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
		require.NoError(t, getErr)
		assert.Equal(t, i, meta.DeleteReapAttempts, "attempt count after sweep %d", i)

		if i < maxDeleteReapAttempts {
			assert.False(t, meta.DeleteReapExhausted, "must not exhaust before maxDeleteReapAttempts")
			// Fast-forward past the next backoff window without sleeping.
			meta.LastDeleteReapAttempt = time.Now().UTC().Add(-2 * deleteReapBackoff(deletingReapMinAge, meta.DeleteReapAttempts))
			require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
		}
	}

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, getErr)
	assert.True(t, meta.DeleteReapExhausted, "the backstop must give up after maxDeleteReapAttempts")
	assert.Equal(t, ClusterStatusDeleting, meta.Status,
		"ADR-0006 §6 billing invariant: an exhausted cluster must stay DELETING")

	terminateCallsAtExhaustion := len(f.inst.terminateCalls)

	// Even long after exhaustion, further sweeps must never re-drive it again.
	meta.LastDeleteReapAttempt = time.Now().UTC().Add(-24 * time.Hour)
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err, "a skipped exhausted cluster must not surface a sweep error")
	assert.Equal(t, 0, n)
	assert.Len(t, f.inst.terminateCalls, terminateCallsAtExhaustion, "an exhausted cluster must never be re-driven again")
}

// TestDeletingReaperSkipsNonDeleting: a CREATING/ACTIVE cluster is never touched.
func TestDeletingReaperSkipsNonDeleting(t *testing.T) {
	f := newDeleteClusterFixture(t, "gamma") // stays CREATING

	reaper := f.svc.NewDeletingReaper()
	n, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "gamma")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusCreating, meta.Status)
	assert.Empty(t, f.inst.terminateCalls)
}
