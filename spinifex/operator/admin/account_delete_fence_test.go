package admin

//test:in-package — the deletion job record, its claim rules and the progress
// tracker are unexported, and the fence is only reachable through them.

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expireLease rewrites the stored record so its holder looks dead.
func expireLease(t *testing.T, jobs jetstream.KeyValue, accountID string) {
	t.Helper()
	job, err := readDeletionJob(t.Context(), jobs, accountID)
	require.NoError(t, err)
	require.NotNil(t, job)
	past := time.Now().UTC().Add(-2 * accountDeletionStale).Format(time.RFC3339)
	job.UpdatedAt, job.LeaseUntil = past, past
	record, err := json.Marshal(job)
	require.NoError(t, err)
	_, err = jobs.Put(t.Context(), accountID, record)
	require.NoError(t, err)
}

func claimAs(t *testing.T, jobs jetstream.KeyValue, accountID, token, holder string) (*accountDeletionJob, uint64, error) {
	t.Helper()
	job, revision, replay, err := claimDeletionJob(t.Context(), jobs, &operatorv1.DeleteAccountRequest{
		AccountID: accountID, AccountName: "tenant@example.com", ClientToken: token,
	}, holder)
	if err == nil {
		require.Nil(t, replay)
	}
	return job, revision, err
}

// A holder that has been taken over must not write again: its next write finds
// the new holder's fence, and it stops and cancels its teardown.
func TestSupersededHolderStopsAndLeavesTheNewHoldersRecord(t *testing.T) {
	t.Parallel()
	gw := newDeleteAccountGateway(t)
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)
	const accountID = "000000000042"

	tokenA, tokenB := strings.Repeat("a", 32), strings.Repeat("b", 32)
	holderA, revisionA, err := claimAs(t, jobs, accountID, tokenA, "node-a")
	require.NoError(t, err)
	var cancelled atomic.Bool
	trackerA := &deletionTracker{jobs: jobs, job: holderA, revision: revisionA, cancel: func() { cancelled.Store(true) }}

	expireLease(t, jobs, accountID)
	holderB, _, err := claimAs(t, jobs, accountID, tokenB, "node-b")
	require.NoError(t, err)
	require.Equal(t, holderA.OperationID, holderB.OperationID, "the operation survives a takeover")
	require.Equal(t, uint64(2), holderB.Fence)

	trackerA.stageFinished(t.Context(), accountteardown.StageResult{Stage: accountteardown.StageCompute})
	assert.True(t, cancelled.Load(), "a superseded holder must cancel its teardown")
	afterStage := describeDeletion(t, gw, accountID)
	assert.Equal(t, uint64(2), afterStage.Fence)
	assert.Equal(t, "node-b", afterStage.Holder)
	assert.Equal(t, tokenB, afterStage.ClientToken)
	assert.Empty(t, afterStage.Stages, "the old holder's stage must not land in the new record")

	trackerA.finished(t.Context(), nil, nil)
	afterFinish := describeDeletion(t, gw, accountID)
	assert.Equal(t, operatorv1.DeletionStateRunning, afterFinish.State, "the old holder cannot declare the operation finished")
	assert.Equal(t, uint64(2), afterFinish.Fence)
}

// A retry carrying the stored token against a RUNNING job whose lease has
// lapsed has no holder to follow, so it takes the job over and resumes it.
func TestSameTokenRetryResumesAStaleRunningJob(t *testing.T) {
	t.Parallel()
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	past := time.Now().UTC().Add(-2 * accountDeletionStale).Format(time.RFC3339)
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "op-abandoned",
		OperationID: "op-abandoned",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       operatorv1.DeletionStateRunning,
		UpdatedAt:   past,
		LeaseUntil:  past,
		Fence:       1,
		Holder:      "node-dead",
	})

	output, err := gw.deleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", deleteClientToken))
	require.NoError(t, err)
	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.Equal(t, "op-abandoned", response.DeletionID, "the operation id is unchanged by a takeover")

	stored := describeDeletion(t, gw, account.AccountID)
	assert.Equal(t, uint64(2), stored.Fence)
	assert.Equal(t, "op-abandoned", stored.OperationID)
	require.Len(t, stored.Takeovers, 1)
	assert.Equal(t, uint64(2), stored.Takeovers[0].Fence)
	assert.Equal(t, takeoverReasonLeaseExpired, stored.Takeovers[0].Reason)
	assert.NotEmpty(t, stored.Takeovers[0].Holder)
}

// A same-token retry against a job whose lease is live still follows it.
func TestSameTokenRetryFollowsALiveJob(t *testing.T) {
	t.Parallel()
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	now := time.Now().UTC()
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "op-live",
		OperationID: "op-live",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       operatorv1.DeletionStateRunning,
		UpdatedAt:   now.Format(time.RFC3339),
		LeaseUntil:  now.Add(accountDeletionStale).Format(time.RFC3339),
		Fence:       1,
		Holder:      "node-live",
	})

	output, err := gw.deleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", deleteClientToken))
	require.NoError(t, err)
	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.Equal(t, "op-live", response.DeletionID)
	assert.Equal(t, uint64(1), describeDeletion(t, gw, account.AccountID).Fence, "no takeover of a live lease")
}

// Two gateways racing for one expired lease: the conditional write lets exactly
// one through, and the other is told the operation is in progress.
func TestConcurrentTakeoverHasExactlyOneWinner(t *testing.T) {
	t.Parallel()
	gw := newDeleteAccountGateway(t)
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)
	const accountID = "000000000042"

	_, _, err = claimAs(t, jobs, accountID, strings.Repeat("a", 32), "node-a")
	require.NoError(t, err)
	expireLease(t, jobs, accountID)

	const takers = 8
	var (
		wg      sync.WaitGroup
		winners atomic.Int32
		losers  atomic.Int32
	)
	for i := range takers {
		wg.Go(func() {
			token := strings.Repeat(string(rune('c'+i)), 32)
			_, _, replay, err := claimDeletionJob(t.Context(), jobs, &operatorv1.DeleteAccountRequest{
				AccountID: accountID, AccountName: "tenant@example.com", ClientToken: token,
			}, "node-"+token[:1])
			switch {
			case err == nil && replay == nil:
				winners.Add(1)
			case err != nil && err.Error() == awserrors.ErrorOperationInProgress:
				losers.Add(1)
			default:
				t.Errorf("unexpected outcome: replay=%v err=%v", replay, err)
			}
		})
	}
	wg.Wait()

	assert.Equal(t, int32(1), winners.Load())
	assert.Equal(t, int32(takers-1), losers.Load())
	stored := describeDeletion(t, gw, accountID)
	assert.Equal(t, uint64(2), stored.Fence)
	assert.Len(t, stored.Takeovers, 1)
}

// A record from a binary that wrote no fence reads as fence 0 and is taken over
// to fence 1.
func TestLegacyRecordWithoutAFenceIsTakenOverToFenceOne(t *testing.T) {
	t.Parallel()
	gw := newDeleteAccountGateway(t)
	jobs := storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "d-legacy",
		AccountID:   "000000000042",
		AccountName: "tenant@example.com",
		ClientToken: strings.Repeat("a", 32),
		State:       operatorv1.DeletionStateRunning,
		UpdatedAt:   time.Now().UTC().Add(-2 * accountDeletionStale).Format(time.RFC3339),
	})

	job, _, err := claimAs(t, jobs, "000000000042", strings.Repeat("b", 32), "node-b")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), job.Fence)
	assert.NotEmpty(t, job.OperationID)
	assert.Equal(t, job.OperationID, job.DeletionID)
	assert.NotEqual(t, "d-legacy", job.DeletionID)
	require.Len(t, job.Takeovers, 1)
	assert.Equal(t, "node-b", job.Takeovers[0].Holder)
}

// A revision conflict caused by this holder's own fence is not a takeover: the
// write is retried at the new revision and the holder carries on.
func TestHeartbeatConflictWithOwnFenceRetries(t *testing.T) {
	t.Parallel()
	gw := newDeleteAccountGateway(t)
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)
	const accountID = "000000000042"

	job, revision, err := claimAs(t, jobs, accountID, strings.Repeat("a", 32), "node-a")
	require.NoError(t, err)
	var cancelled atomic.Bool
	tracker := &deletionTracker{jobs: jobs, job: job, revision: revision, cancel: func() { cancelled.Store(true) }}

	// The revision moves under the tracker without the fence changing.
	record, err := json.Marshal(job)
	require.NoError(t, err)
	moved, err := jobs.Put(t.Context(), accountID, record)
	require.NoError(t, err)
	require.Greater(t, moved, revision)

	tracker.stageFinished(t.Context(), accountteardown.StageResult{Stage: accountteardown.StageCompute})

	assert.False(t, cancelled.Load())
	stored := describeDeletion(t, gw, accountID)
	require.Len(t, stored.Stages, 1)
	assert.Equal(t, uint64(1), stored.Fence)
	assert.NotEmpty(t, stored.LeaseUntil, "the heartbeat renews the lease")
}
