package admin

//test:in-package — the resumer's eligibility rule and the takeover it shares
// with a retried request are unexported.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func abandonedJob(accountID, name string) *accountDeletionJob {
	past := time.Now().UTC().Add(-2 * accountDeletionStale).Format(time.RFC3339)
	return &accountDeletionJob{
		DeletionID:  "op-abandoned",
		OperationID: "op-abandoned",
		AccountID:   accountID,
		AccountName: name,
		ClientToken: deleteClientToken,
		State:       operatorv1.DeletionStateRunning,
		UpdatedAt:   past,
		LeaseUntil:  past,
		Fence:       1,
		Holder:      "node-dead",
	}
}

// ADR-0003:S7: the resumer is an automatic actor that can end in deleting an
// account's data, so it may only complete a deletion the coordinator already
// accepted. A record without an operation id and a fence was never accepted, a
// live lease still has a holder, and a finished or failed job is not its to
// restart.
func TestS7_DeletionResumerActsOnlyOnAnAcceptedOperationWithoutALiveLease(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(accountDeletionStale).Format(time.RFC3339)

	cases := map[string]func(*accountDeletionJob){
		"no operation id": func(j *accountDeletionJob) { j.OperationID = "" },
		"no fence":        func(j *accountDeletionJob) { j.Fence = 0 },
		"live lease": func(j *accountDeletionJob) {
			j.LeaseUntil, j.UpdatedAt = future, now.Format(time.RFC3339)
		},
		"failed job":    func(j *accountDeletionJob) { j.State = operatorv1.DeletionStateFailed },
		"completed job": func(j *accountDeletionJob) { j.State = operatorv1.DeletionStateCompleted },
		"legacy record": func(j *accountDeletionJob) { j.OperationID, j.Fence, j.LeaseUntil = "", 0, "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			gw := newDeleteAccountGateway(t)
			account := newTenantAccount(t, gw, "tenant@example.com")
			job := abandonedJob(account.AccountID, "tenant@example.com")
			mutate(job)
			jobs := storeDeletionJob(t, gw, job)
			before, err := jobs.Get(t.Context(), account.AccountID)
			require.NoError(t, err)

			assert.Zero(t, gw.resumeDeletions(t.Context(), now), "resumed a record it must not touch")

			after, err := jobs.Get(t.Context(), account.AccountID)
			require.NoError(t, err)
			assert.Equal(t, before.Revision(), after.Revision(), "the record must be left unwritten")
		})
	}

	t.Run("control: accepted operation with an expired lease is resumed", func(t *testing.T) {
		gw := newDeleteAccountGateway(t)
		account := newTenantAccount(t, gw, "tenant@example.com")
		storeDeletionJob(t, gw, abandonedJob(account.AccountID, "tenant@example.com"))

		assert.Equal(t, 1, gw.resumeDeletions(t.Context(), now))
	})
}

// A job left by a dead gateway finishes with no request: the resumer takes it
// over once, under the same operation, and the teardown runs to completion.
func TestDeletionResumerFinishesAnAbandonedJobWithoutARequest(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	storeDeletionJob(t, gw, abandonedJob(account.AccountID, "tenant@example.com"))

	require.Equal(t, 1, gw.resumeDeletions(t.Context(), time.Now().UTC()))

	require.Eventually(t, func() bool {
		return describeDeletion(t, gw, account.AccountID).State == operatorv1.DeletionStateCompleted
	}, 30*time.Second, 100*time.Millisecond)

	stored := describeDeletion(t, gw, account.AccountID)
	assert.Equal(t, "op-abandoned", stored.OperationID)
	assert.Equal(t, uint64(2), stored.Fence)
	require.Len(t, stored.Takeovers, 1)
	assert.Equal(t, takeoverReasonLeaseExpired, stored.Takeovers[0].Reason)
	assert.Equal(t, deleteClientToken, stored.ClientToken, "the original token is kept")
}

// Every gateway runs a resumer, so several see the same expired lease at once.
func TestConcurrentResumersProduceOneTakeover(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	storeDeletionJob(t, gw, abandonedJob(account.AccountID, "tenant@example.com"))

	const resumers = 8
	var (
		wg      sync.WaitGroup
		resumed atomic.Int32
	)
	for range resumers {
		wg.Go(func() { resumed.Add(int32(gw.resumeDeletions(t.Context(), time.Now().UTC()))) })
	}
	wg.Wait()

	assert.Equal(t, int32(1), resumed.Load())
	require.Eventually(t, func() bool {
		return describeDeletion(t, gw, account.AccountID).State == operatorv1.DeletionStateCompleted
	}, 30*time.Second, 100*time.Millisecond)
	stored := describeDeletion(t, gw, account.AccountID)
	assert.Equal(t, uint64(2), stored.Fence)
	assert.Len(t, stored.Takeovers, 1)
}

func TestDeletionResumerStopsWithItsContext(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	stop := gw.StartDeletionResumer(t.Context())
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resumer did not stop")
	}
}
