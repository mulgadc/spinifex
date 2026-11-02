package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markDeleting records a seeded cluster as DELETING without starting its
// teardown, standing in for a delete whose kick died, and backdates it by age.
func markDeleting(t *testing.T, f *deleteClusterFixture, name string, age time.Duration) {
	t.Helper()
	require.NoError(t, SetClusterStatus(t.Context(), f.kv, name, ClusterStatusDeleting))
	meta, err := GetClusterMeta(t.Context(), f.kv, name)
	require.NoError(t, err)
	meta.DeletingSince = time.Now().UTC().Add(-age)
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
}

// backdateLastAttempt moves the last teardown pass past its backoff window.
func backdateLastAttempt(t *testing.T, f *deleteClusterFixture, name string) *ClusterMeta {
	t.Helper()
	meta, err := GetClusterMeta(t.Context(), f.kv, name)
	require.NoError(t, err)
	meta.LastDeleteReapAttempt = time.Now().UTC().Add(-2 * deleteReapBackoff(deletingReapMinAge, meta.DeleteReapAttempts))
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
	return meta
}

// sweepAndSettle runs one reaper sweep and waits for what it handed off.
func sweepAndSettle(t *testing.T, f *deleteClusterFixture) int {
	t.Helper()
	n, err := f.svc.NewDeletingReaper().Sweep(context.Background())
	require.NoError(t, err)
	f.svc.WaitTeardowns()
	return n
}

// TestRLC4_DeletingReaperReDrivesWedgedTeardown: a DELETING record whose
// teardown is not running (its kick died with the daemon) is finished by the
// reaper, infra torn down and record swept, so its billable EIP is not stranded.
func TestRLC4_DeletingReaperReDrivesWedgedTeardown(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	markDeleting(t, f, "alpha", 10*time.Minute)

	assert.Equal(t, 1, sweepAndSettle(t, f), "the recorded delete must be handed to a converger")

	_, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound, "meta must be swept once the teardown completes")
	assert.GreaterOrEqual(t, len(f.inst.terminated()), 1, "CP VM must be terminated")
	assert.Len(t, f.eip.releaseCalls, 1, "the billable egress EIP must be released, not stranded")
}

// TestDeletingReaperLeavesAHeldTeardownAlone: a teardown another node is
// running holds the lease, so the sweep starts nothing.
func TestDeletingReaperLeavesAHeldTeardownAlone(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "beta")
	markDeleting(t, f, "beta", time.Second)
	_, err := f.svc.leaderKV.Create(t.Context(), teardownLeaderKey(testAccountID, "beta"), []byte("node-2"))
	require.NoError(t, err)

	assert.Equal(t, 0, sweepAndSettle(t, f))

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "beta")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.Empty(t, f.inst.terminated(), "the lease holder owns the teardown")
}

// TestDeletingReaper_EnumerationFailureIsReported: the KV bucket lister closes
// its channel the same way whether the listing completed or failed, so a sweep
// that ignored the terminal error would report "nothing wedged" for an
// unreachable JetStream.
func TestDeletingReaper_EnumerationFailureIsReported(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "delta")
	markDeleting(t, f, "delta", 10*time.Minute)

	// Closing the connection fails the stream-names request behind the listing.
	f.svc.deps.NATSConn.Close()

	n, err := f.svc.NewDeletingReaper().Sweep(context.Background())
	require.Error(t, err, "a failed bucket enumeration must not report a completed sweep")
	assert.Zero(t, n)
}

// TestDeletingReaper_BacksOffAfterFailedAttempt: a failing teardown is not
// re-run on every GC tick; the next pass waits out the backoff measured from
// the persisted attempt time, so the window survives a restart.
func TestDeletingReaper_BacksOffAfterFailedAttempt(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")
	markDeleting(t, f, "alpha", 10*time.Minute)

	assert.Equal(t, 1, sweepAndSettle(t, f))
	assert.Len(t, f.inst.terminated(), 1, "the first pass must run")
	meta, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, 1, meta.DeleteReapAttempts, "the attempt must be counted")
	assert.False(t, meta.LastDeleteReapAttempt.IsZero(), "the attempt time must be stamped")

	assert.Equal(t, 0, sweepAndSettle(t, f), "inside the backoff window nothing is started")
	assert.Len(t, f.inst.terminated(), 1)

	backdateLastAttempt(t, f, "alpha")
	assert.Equal(t, 1, sweepAndSettle(t, f))
	assert.Len(t, f.inst.terminated(), 2, "the next pass runs once the window elapses")
	meta, getErr = GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, 2, meta.DeleteReapAttempts)
}

// TestDeletingReaper_KeepsRetryingPastTheOldAttemptLimit: once the request no
// longer reports a teardown failure, abandoning the record would hold its
// capacity behind a 200 forever. Past maxDeleteReapAttempts the record is
// flagged for an operator, stays DELETING, and is still retried.
func TestDeletingReaper_KeepsRetryingPastTheOldAttemptLimit(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("permanent hypervisor failure")
	markDeleting(t, f, "alpha", 10*time.Minute)

	for i := 1; i <= maxDeleteReapAttempts+2; i++ {
		assert.Equal(t, 1, sweepAndSettle(t, f), "pass %d must be started", i)
		backdateLastAttempt(t, f, "alpha")
	}

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, maxDeleteReapAttempts+2, meta.DeleteReapAttempts)
	assert.True(t, meta.DeleteReapExhausted, "a long-failing teardown is flagged for an operator")
	assert.Equal(t, ClusterStatusDeleting, meta.Status,
		"ADR-0003:S2: incomplete cleanup keeps its record DELETING")
	assert.Len(t, f.inst.terminated(), maxDeleteReapAttempts+2, "every pass retried the teardown")
}

// TestDeletingReaper_ResumesAnAlreadyExhaustedRecord: a record an older build
// flagged exhausted and stopped retrying is picked up and finished.
func TestDeletingReaper_ResumesAnAlreadyExhaustedRecord(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	markDeleting(t, f, "alpha", 48*time.Hour)
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	meta.DeleteReapAttempts = maxDeleteReapAttempts
	meta.DeleteReapExhausted = true
	meta.LastDeleteReapAttempt = time.Now().UTC().Add(-24 * time.Hour)
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))

	sweepAndSettle(t, f)

	_, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound, "an exhausted record must be retried and, once it succeeds, swept")
	assert.Len(t, f.inst.terminated(), 1)
}

// TestDeletingReaper_PersistsLastErrorOnFailure: the failure reason is on the
// record from the first failed pass, so an operator sees why it is stuck.
func TestDeletingReaper_PersistsLastErrorOnFailure(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("dependency violation: eni still attached")
	markDeleting(t, f, "alpha", 10*time.Minute)

	sweepAndSettle(t, f)

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Contains(t, meta.LastDeleteReapError, "dependency violation")
	assert.Equal(t, stepControlPlane, meta.DeleteFailedStep)
}

// TestDeletingReaperSkipsNonDeleting: a CREATING/ACTIVE cluster is never touched.
func TestDeletingReaperSkipsNonDeleting(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "gamma") // stays CREATING

	assert.Equal(t, 0, sweepAndSettle(t, f))

	meta, getErr := GetClusterMeta(t.Context(), f.kv, "gamma")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusCreating, meta.Status)
	assert.Empty(t, f.inst.terminated())
}

func TestDeleteReapBackoffIsCapped(t *testing.T) {
	t.Parallel()
	assert.Zero(t, deleteReapBackoff(time.Minute, 0))
	assert.Equal(t, time.Minute, deleteReapBackoff(time.Minute, 1))
	assert.Equal(t, 4*time.Minute, deleteReapBackoff(time.Minute, 3))
	assert.Equal(t, deleteReapBackoffCap, deleteReapBackoff(time.Minute, 200), "no overflow, and never past the cap")
}
