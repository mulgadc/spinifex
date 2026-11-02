package admin

//test:in-package — the exercise runs through the job record, the takeover and
// the resumer, all of which are unexported.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const exerciseToken = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"

type panicCleaner struct{ calls int }

func (p *panicCleaner) CleanupPlatform(context.Context, *accountcleanup.Request) (*accountcleanup.Result, error) {
	p.calls++
	return &accountcleanup.Result{}, nil
}

func exerciseBody(t *testing.T, accountID, name, token string, mutate func(*operatorv1.DeleteAccountRequest)) []byte {
	t.Helper()
	req := operatorv1.DeleteAccountRequest{AccountID: accountID, AccountName: name, ClientToken: token, NonFinalizing: true}
	if mutate != nil {
		mutate(&req)
	}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return body
}

func awaitState(t *testing.T, gw Deps, accountID, state string) *accountDeletionJob {
	t.Helper()
	require.Eventually(t, func() bool {
		return describeDeletion(t, gw, accountID).State == state
	}, 30*time.Second, 100*time.Millisecond)
	return describeDeletion(t, gw, accountID)
}

// An exercise ends EXERCISED with the account still TERMINATING, and a later
// ordinary delete takes the same operation over to finish it.
func TestNonFinalizingDeleteEndsExercisedAndALaterDeleteTakesItOver(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	gw.PlatformCleaner = &panicCleaner{}
	account := newTenantAccount(t, gw, "tenant@example.com")

	_, err := gw.deleteAccount(t.Context(), exerciseBody(t, account.AccountID, "tenant@example.com", exerciseToken, nil))
	require.NoError(t, err)

	exercised := awaitState(t, gw, account.AccountID, operatorv1.DeletionStateExercised)
	require.NotNil(t, exercised.IAM)
	require.NotNil(t, exercised.Exercise)
	assert.True(t, exercised.NonFinalizing)
	assert.NotEmpty(t, exercised.FinishedAt)
	assert.Empty(t, exercised.Stages, "no legacy stage runs")
	stored, err := gw.IAM.GetAccount(account.AccountID)
	require.NoError(t, err)
	assert.Equal(t, "TERMINATING", stored.Status)

	_, err = gw.deleteAccount(t.Context(), deleteAccountBody(t, account.AccountID, "tenant@example.com", deleteClientToken))
	require.NoError(t, err)

	done := awaitState(t, gw, account.AccountID, operatorv1.DeletionStateCompleted)
	assert.Equal(t, exercised.OperationID, done.OperationID)
	assert.Equal(t, uint64(2), done.Fence)
	require.Len(t, done.Takeovers, 1)
	assert.Equal(t, takeoverReasonExercised, done.Takeovers[0].Reason)
	assert.Nil(t, done.Exercise, "the takeover starts a fresh run")
	assert.Equal(t, uint64(2), done.IAM.Fence)
	assert.Empty(t, done.IAM.QuiescedScope, "an ordinary delete records no scope")
}

func TestNonFinalizingIsPartOfTheIdempotencyKeyAndRefusesForce(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	gw.PlatformCleaner = &panicCleaner{}
	account := newTenantAccount(t, gw, "tenant@example.com")

	_, err := gw.deleteAccount(t.Context(), exerciseBody(t, account.AccountID, "tenant@example.com", exerciseToken,
		func(r *operatorv1.DeleteAccountRequest) { r.Force = true }))
	require.EqualError(t, err, awserrors.ErrorInvalidParameterCombination)

	_, err = gw.deleteAccount(t.Context(), exerciseBody(t, account.AccountID, "tenant@example.com", exerciseToken, nil))
	require.NoError(t, err)
	awaitState(t, gw, account.AccountID, operatorv1.DeletionStateExercised)

	_, err = gw.deleteAccount(t.Context(), deleteAccountBody(t, account.AccountID, "tenant@example.com", exerciseToken))
	require.EqualError(t, err, awserrors.ErrorIdempotentParameterMismatch, "the same token cannot turn an exercise into a delete")

	again, err := gw.deleteAccount(t.Context(), exerciseBody(t, account.AccountID, "tenant@example.com", exerciseToken, nil))
	require.NoError(t, err)
	assert.Equal(t, operatorv1.DeletionStateExercised, again.(*DeleteAccountResponse).State, "a repeated exercise is answered from the record")
}

func TestNonFinalizingWithoutACleanupIsRefusedBeforeAnyJobExists(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")

	_, err := gw.deleteAccount(t.Context(), exerciseBody(t, account.AccountID, "tenant@example.com", exerciseToken, nil))

	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)
	stored, err := readDeletionJob(t.Context(), jobs, account.AccountID)
	require.NoError(t, err)
	assert.Nil(t, stored)
}

// A resumed exercise stays an exercise: the resumer must not turn a recorded
// non-finalizing decision into a deletion.
func TestResumerKeepsAnAbandonedExerciseNonFinalizing(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	gw.PlatformCleaner = &panicCleaner{}
	account := newTenantAccount(t, gw, "tenant@example.com")
	job := abandonedJob(account.AccountID, "tenant@example.com")
	job.NonFinalizing = true
	storeDeletionJob(t, gw, job)

	require.Equal(t, 1, gw.resumeDeletions(t.Context(), time.Now().UTC()))

	done := awaitState(t, gw, account.AccountID, operatorv1.DeletionStateExercised)
	assert.Equal(t, uint64(2), done.Fence)
	assert.True(t, done.NonFinalizing)
	stored, err := gw.IAM.GetAccount(account.AccountID)
	require.NoError(t, err)
	assert.Equal(t, "TERMINATING", stored.Status, "the account record is never removed")
}
