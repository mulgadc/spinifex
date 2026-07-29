package handlers_rds

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A lease held by a worker that is not this one, which is what a modify still
// inside its own API call looks like from a reconcile pass.
func heldLease(remaining time.Duration) *ModifyLease {
	return &ModifyLease{Holder: "node-b/deadbeef", ExpiresAt: time.Now().UTC().Add(remaining)}
}

// The failure the lease exists to stop: ApplyImmediately runs the change inline
// for as long as a grow or a replace takes, and the reconciler sweeps every
// modifying instance looking for exactly that record shape. Without the lease
// the sweep runs a second grow — and for a class change, a second
// replaceInstanceVM against the same data volume and endpoint ENI.
func TestModifyDBInstance_HoldsTheLeaseSoAReconcilePassCannotReEnterTheChange(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	// Fired from inside the resize, which is the widest point of the window: the
	// VM is down, the volume is being taken, and the record still names the
	// change as outstanding. Once, or a re-entered pass would recurse here.
	var sweep sync.Once
	var swept error
	h.storage.onModify = func() {
		sweep.Do(func() { swept = h.rec.reconcileOnce(t.Context()) })
	}

	in := modifyInput()
	in.AllocatedStorage, in.ApplyImmediately = aws.Int64(50), aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)
	require.NoError(t, swept)

	assert.Len(t, h.storage.modified, 1, "the reconcile pass must not run a second grow")
	assert.Equal(t, []string{"stop:" + testInstance, "start:" + testInstance}, h.cmdr.calls,
		"the reconcile pass must not open a second stop/start cycle")
}

// A pass that cannot take the lease leaves the instance alone entirely: the
// holder is working on it, and the record already says what it is becoming.
func TestReconciler_LeavesAModifyItsHolderIsStillInside(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()})
	rec.ModifyLease = heldLease(modifyLeaseTTL)
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	assert.Empty(t, h.storage.modified)
	assert.Empty(t, h.cmdr.calls)
	stored := h.record(t)
	assert.Equal(t, StatusModifying, stored.Status)
	assert.Equal(t, rec.ModifyLease.Holder, stored.ModifyLease.Holder, "the holder's lease is left as it is")
}

// Nobody is renewing an abandoned lease, so it expires and the next pass takes
// the change over — which is what makes a leader that died mid-modify
// recoverable rather than terminal.
func TestReconciler_ResumesAModifyWhoseHolderIsGone(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()})
	rec.ModifyLease = heldLease(-time.Second)
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	assert.Equal(t, int64(50), h.storage.sizes[testDataVolume])
	assert.Equal(t, int64(50), h.record(t).AllocatedStorage)
}

// The lease is released as soon as the work finishes rather than left to time
// out, or the pass that has to finish the grow inside the guest would wait a
// whole TTL for a change that already landed.
func TestApplyPendingModifications_ReleasesTheLeaseWhateverTheOutcome(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		h := newModifyHarness(t)
		seedInstance(t, h.svc, modifyingRecord(
			&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()}))

		require.NoError(t, h.rec.reconcileOnce(t.Context()))
		assert.Nil(t, h.record(t).ModifyLease)
	})

	t.Run("failed", func(t *testing.T) {
		h := newModifyHarness(t)
		h.storage.modifyErr = errors.New("the volume store is unavailable")
		seedInstance(t, h.svc, modifyingRecord(
			&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()}))

		require.NoError(t, h.rec.reconcileOnce(t.Context()))
		assert.Nil(t, h.record(t).ModifyLease, "a failed attempt must not hold the lease against its own retry")
	})
}

// A holder that renews but never finishes would otherwise keep the instance in
// modifying forever, since a pass that cannot take the lease does nothing. Past
// the budget it is failed anyway, which is the state a retry is legal from.
func TestReconciler_FailsAModifyWhoseHolderOverrunsTheBudget(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifyingRecord(&PendingModifiedValues{AllocatedStorage: aws.Int64(50), RequestedAt: time.Now().UTC()})
	rec.ModifyLease = heldLease(modifyLeaseTTL)
	started := time.Now().UTC().Add(-2 * transitionTimeout)
	rec.TransitionStartedAt = &started
	seedInstance(t, h.svc, rec)

	require.NoError(t, h.rec.reconcileOnce(t.Context()))

	stored := h.record(t)
	assert.Equal(t, StatusFailed, stored.Status)
	assert.Contains(t, stored.FailureReason, "still being modified")
	require.NotNil(t, stored.PendingModifiedValues, "the request stays recorded so the retry has something to run")
}

// Re-taking a lease we already hold has to be allowed, or a caller that claims
// twice deadlocks against itself.
func TestClaimModifyLease_RefusesAnotherHolderButNotItsOwn(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())
	kv := h.kv(t)

	claimed, err := h.svc.claimModifyLease(t.Context(), kv, testDBID, "node-a/0001")
	require.NoError(t, err)
	assert.True(t, claimed)

	claimed, err = h.svc.claimModifyLease(t.Context(), kv, testDBID, "node-b/0002")
	require.NoError(t, err)
	assert.False(t, claimed, "a live lease belongs to its holder alone")

	claimed, err = h.svc.claimModifyLease(t.Context(), kv, testDBID, "node-a/0001")
	require.NoError(t, err)
	assert.True(t, claimed)
}
