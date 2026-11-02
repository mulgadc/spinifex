package admin

//test:in-package — the deletion job record, its claim rules and the progress
// tracker are unexported, and the fence gaps pinned here are only reachable
// through them.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CURRENT: every progress write is a plain Put, so a holder that has been taken
// over keeps overwriting the new holder's record. The record then names the old
// operation and can even report it COMPLETED while the new holder still runs.
func TestCurrentBehaviour_SupersededHolderOverwritesTheNewHoldersRecord(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)
	const accountID = "000000000042"

	tokenA := strings.Repeat("a", 32)
	holderA, replay, err := claimDeletionJob(t.Context(), jobs, &operatorv1.DeleteAccountRequest{
		AccountID: accountID, AccountName: "tenant@example.com", ClientToken: tokenA,
	})
	require.NoError(t, err)
	require.Nil(t, replay)
	trackerA := &deletionTracker{jobs: jobs, job: holderA}

	// A stops heartbeating: its record goes stale.
	stale := *holderA
	stale.UpdatedAt = time.Now().UTC().Add(-2 * accountDeletionStale).Format(time.RFC3339)
	record, err := json.Marshal(&stale)
	require.NoError(t, err)
	_, err = jobs.Put(t.Context(), accountID, record)
	require.NoError(t, err)

	// B takes over under a new token and its own deletion id.
	tokenB := strings.Repeat("b", 32)
	holderB, replay, err := claimDeletionJob(t.Context(), jobs, &operatorv1.DeleteAccountRequest{
		AccountID: accountID, AccountName: "tenant@example.com", ClientToken: tokenB,
	})
	require.NoError(t, err)
	require.Nil(t, replay)
	require.NotEqual(t, holderA.DeletionID, holderB.DeletionID)
	require.Equal(t, holderB.DeletionID, describeDeletion(t, gw, accountID).DeletionID)

	// A was never told. Its next progress write lands over B's record.
	trackerA.stageFinished(t.Context(), accountteardown.StageResult{Stage: accountteardown.StageCompute})
	afterStage := describeDeletion(t, gw, accountID)
	assert.Equal(t, holderA.DeletionID, afterStage.DeletionID, "the superseded holder replaced the new holder's record")
	assert.Equal(t, tokenA, afterStage.ClientToken)

	// And its final write declares the operation finished while B has not.
	trackerA.finished(t.Context(), nil, nil)
	afterFinish := describeDeletion(t, gw, accountID)
	assert.Equal(t, operatorv1.DeletionStateCompleted, afterFinish.State)
	assert.Equal(t, holderA.DeletionID, afterFinish.DeletionID)
}

// CURRENT: a retry carrying the stored client token is answered from the record
// before takeover is considered, so a dead RUNNING job is followed indefinitely
// and nothing resumes it. Only a different token takes over.
func TestCurrentBehaviour_SameTokenRetryFollowsAStaleRunningJob(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	staleAt := time.Now().UTC().Add(-2 * accountDeletionStale).Format(time.RFC3339)
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "d-abandoned",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       operatorv1.DeletionStateRunning,
		UpdatedAt:   staleAt,
	})

	output, err := gw.deleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", deleteClientToken))
	require.NoError(t, err)
	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.Equal(t, "d-abandoned", response.DeletionID, "answered from the dead record")
	assert.Equal(t, operatorv1.DeletionStateRunning, response.State)

	// No teardown started: the record is untouched and the account was never
	// moved to TERMINATING.
	assert.Never(t, func() bool {
		stored := describeDeletion(t, gw, account.AccountID)
		current, err := gw.IAM.GetAccount(account.AccountID)
		return err != nil || stored.DeletionID != "d-abandoned" || stored.UpdatedAt != staleAt ||
			current.Status != "ACTIVE"
	}, 300*time.Millisecond, 20*time.Millisecond)
}
