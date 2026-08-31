package handlers_rds

//test:in-package — asserts on the deadline reconcileOnce returns, which is
// unexported and is the whole subject of these tests.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settledHeartbeatBound is how long after its last beat a settled instance is
// still believed. It is what a settled record's deadline is measured against.
const settledHeartbeatBound = HeartbeatStaleAfter + HeartbeatPersistFloor

// A transitional instance is waiting on an EC2 lifecycle state that no KV write
// announces, so there is no update for a watch to deliver and the pass has to
// ask to come back.
func TestReconcileOnce_ATransitionalInstanceAsksForTheShortInterval(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	h.state.state = "pending"
	seedInstance(t, h.svc, healthyRecord())

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	status, _ := h.statusOf(t, testDBID)
	require.Equal(t, StatusCreating, status, "the VM is not running, so this pass must not settle it")
	assert.Equal(t, reconcileInterval, revisit)
}

// A settled instance is waiting for its agent to fall silent, and silence writes
// nothing. The deadline is the instant the beat expires, which is sharper than
// the ticker that used to notice it up to one interval late.
func TestReconcileOnce_ASettledInstanceAsksForItsHeartbeatDeadline(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	rec := healthyRecord()
	rec.Status = StatusAvailable
	seedInstance(t, h.svc, rec)

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	status, _ := h.statusOf(t, testDBID)
	require.Equal(t, StatusAvailable, status)
	assert.Greater(t, revisit, settledHeartbeatBound-time.Minute,
		"a beat seconds old must not be treated as nearly stale")
	assert.LessOrEqual(t, revisit, settledHeartbeatBound)
}

// The window between a beat expiring and the instance being called failed is
// where the whole detector lives, and nothing writes to KV during it. Returning
// no deadline here left the pass that does the failing with nothing to schedule
// it, so a dead database waited for the resync — slower than the ticker it
// replaced, and the exact regression this design exists to avoid.
func TestReconcileOnce_ADarkInstanceAsksForItsFailureGrace(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	rec := healthyRecord()
	rec.Status = StatusAvailable
	stale := time.Now().UTC().Add(-2 * settledHeartbeatBound)
	rec.Agent.LastSeen = &stale
	seedInstance(t, h.svc, rec)

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Positive(t, revisit, "an expired beat must not read as nothing left to wait for")
	assert.LessOrEqual(t, revisit, h.svc.failureGrace())
}

// Partway through the grace window the deadline is what remains of it, not the
// whole window again — otherwise each pass would push the failure further out.
func TestReconcileOnce_AFailureClockAlreadyRunningAsksForWhatIsLeftOfIt(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	rec := healthyRecord()
	rec.Status = StatusAvailable
	stale := time.Now().UTC().Add(-2 * settledHeartbeatBound)
	rec.Agent.LastSeen = &stale
	started := time.Now().UTC().Add(-h.svc.failureGrace() / 2)
	rec.UnhealthySince = &started
	seedInstance(t, h.svc, rec)

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Positive(t, revisit)
	assert.LessOrEqual(t, revisit, h.svc.failureGrace()/2)
}

// A failed instance has no clock left to run. Recovery is a heartbeat, which is
// a KV write the watch already delivers.
func TestReconcileOnce_AFailedInstanceAsksForNoDeadline(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	rec := healthyRecord()
	rec.Status = StatusFailed
	stale := time.Now().UTC().Add(-2 * settledHeartbeatBound)
	rec.Agent.LastSeen = &stale
	seedInstance(t, h.svc, rec)

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Zero(t, revisit)
}

// The earliest across the fleet is the one that matters: one instance mid-create
// sets the pace for the pass regardless of how many settled ones sit beside it.
func TestReconcileOnce_TheEarliestDeadlineAcrossTheFleetWins(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	h.state.state = "pending"

	settled := healthyRecord()
	settled.Status = StatusAvailable
	seedInstance(t, h.svc, settled)

	transitional := healthyRecord()
	transitional.DBInstanceIdentifier = "db-still-creating"
	seedInstance(t, h.svc, transitional)

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Equal(t, reconcileInterval, revisit)
}

// A region with nothing in it stops asking altogether, which is the case the
// whole change is for: the loop falls back to the resync and nothing else.
func TestReconcileOnce_NothingToDoAsksForNoDeadline(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Zero(t, revisit)
}

// A snapshot record left in creating is settled against EC2 once its resolve
// timeout elapses. Until then the pass asks to be back at that instant rather
// than leaving a dead worker's record to the resync.
func TestReconcileOnce_ACreatingSnapshotAsksForItsResolveDeadline(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	creating := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC().Add(-snapshotResolveTimeout + 2*time.Minute),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &creating))

	revisit, err := h.rec.reconcileOnce(t.Context())
	require.NoError(t, err)

	assert.Greater(t, revisit, time.Minute, "the resolve timeout has two minutes left to run")
	assert.LessOrEqual(t, revisit, 2*time.Minute)
}

// A follower does no work but cannot be left waiting on a watch either: the
// lease turning over is not a KV write it sees.
func TestReconcilePass_AFollowerAsksAgainAtTheLeaseRate(t *testing.T) {
	t.Parallel()
	h := newReconcileHarness(t)
	seedInstance(t, h.svc, healthyRecord())

	revisit, err := h.rec.reconcilePass(t.Context())
	require.NoError(t, err)

	status, _ := h.statusOf(t, testDBID)
	assert.Equal(t, StatusCreating, status, "a follower must not reconcile")
	assert.Equal(t, leaseRefresh, revisit)
}
