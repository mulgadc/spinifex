package gateway

//test:in-package — the deletion job record, its claim rules and the progress
// tracker are unexported, and they are what make a teardown survive a retry or
// a gateway restart. None of that is observable from the HTTP surface alone.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/accountteardown"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deleteClientToken = "8b9c1d2e3f4a5b6c7d8e9f0a1b2c3d4e"

// newDeleteAccountGateway wires a gateway against a real IAM service and
// embedded JetStream, with a stub cluster answering the EC2 subjects the
// reapers drive. The tenant has no resources, so every EC2 stage drains empty
// and the test exercises the job record rather than the reapers.
func newDeleteAccountGateway(t *testing.T) *GatewayConfig {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := handlers_iam.NewIAMServiceImpl(t.Context(), nc, masterKey, 1)
	require.NoError(t, err)

	sub, err := nc.Subscribe("ec2.>", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	return &GatewayConfig{
		DisableLogging: true,
		IAMService:     svc,
		NATSConn:       nc,
		ExpectedNodes:  1,
	}
}

// newTenantAccount creates an account that is safe to delete. Account ids are
// sequential and the first belongs to the super admin, which is protected, so
// the tenant under test can never be the first one created.
func newTenantAccount(t *testing.T, gw *GatewayConfig, name string) *handlers_iam.Account {
	t.Helper()
	if accounts, err := gw.IAMService.ListAccounts(); err == nil && len(accounts) == 0 {
		_, err := gw.IAMService.CreateAccount("super-admin@example.com")
		require.NoError(t, err)
	}
	account, err := gw.IAMService.CreateAccount(name)
	require.NoError(t, err)
	return account
}

func deleteAccountBody(t *testing.T, accountID, name, token string) []byte {
	t.Helper()
	body, err := json.Marshal(DeleteAccountRequest{
		AccountID: accountID, AccountName: name, ClientToken: token,
	})
	require.NoError(t, err)
	return body
}

// storeDeletionJob seeds a job record directly, which is how a job left behind
// by another gateway is reproduced without racing a live teardown.
func storeDeletionJob(t *testing.T, gw *GatewayConfig, job *accountDeletionJob) jetstream.KeyValue {
	t.Helper()
	jobs, err := gw.accountDeletionStore(t.Context())
	require.NoError(t, err)

	record, err := json.Marshal(job)
	require.NoError(t, err)
	_, err = jobs.Put(t.Context(), job.AccountID, record)
	require.NoError(t, err)
	return jobs
}

func describeDeletion(t *testing.T, gw *GatewayConfig, accountID string) *accountDeletionJob {
	t.Helper()
	body, err := json.Marshal(DescribeAccountDeletionRequest{AccountID: accountID})
	require.NoError(t, err)

	output, err := gw.adminDescribeAccountDeletion(t.Context(), body)
	require.NoError(t, err)
	job, ok := output.(*accountDeletionJob)
	require.True(t, ok)
	return job
}

// A dry run is the operator's look before the leap. It must report what would
// go and leave the account untouched — including its status, since TERMINATING
// is a one-way door.
func TestAdminDeleteAccountDryRunChangesNothing(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")

	body, err := json.Marshal(DeleteAccountRequest{AccountID: account.AccountID, DryRun: true})
	require.NoError(t, err)

	output, err := gw.adminDeleteAccount(t.Context(), body)
	require.NoError(t, err)

	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.True(t, response.DryRun)
	assert.Equal(t, "DRY_RUN", response.State)
	assert.Empty(t, response.DeletionID, "a dry run must not claim a job")
	require.NotNil(t, response.Inventory)

	stored, err := gw.IAMService.GetAccount(account.AccountID)
	require.NoError(t, err)
	assert.Equal(t, handlers_iam.AccountStatusActive, stored.Status)

	// No job record means a later real delete is not answered as a replay.
	_, err = gw.adminDescribeAccountDeletion(t.Context(),
		[]byte(`{"accountId":"`+account.AccountID+`"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())
}

// The request returns as soon as the job is claimed. A teardown takes minutes
// and holding the call open for it would make every timeout-and-retry start a
// second one.
func TestAdminDeleteAccountReturnsBeforeTheTeardownFinishes(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")

	output, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", deleteClientToken))
	require.NoError(t, err)

	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.NotEmpty(t, response.DeletionID)
	assert.Equal(t, account.AccountID, response.AccountID)
	assert.Equal(t, DeletionStateRunning, response.State)

	// Progress is polled, so the record has to reach a terminal state on its own.
	require.Eventually(t, func() bool {
		return describeDeletion(t, gw, account.AccountID).State == DeletionStateCompleted
	}, 60*time.Second, 100*time.Millisecond)

	job := describeDeletion(t, gw, account.AccountID)
	assert.Equal(t, response.DeletionID, job.DeletionID)
	assert.NotEmpty(t, job.FinishedAt)
	assert.Empty(t, job.Error)
	assert.NotEmpty(t, job.Stages, "the stage report is the only view into a finished teardown")

	_, err = gw.IAMService.GetAccount(account.AccountID)
	assert.Error(t, err, "the account record must be gone once the job completes")
}

// The record outlives the account it describes, so a retry that arrives after
// the teardown finished is answered from it rather than as a missing account.
func TestAdminDeleteAccountReplaysAfterTheAccountIsGone(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	body := deleteAccountBody(t, account.AccountID, "tenant@example.com", deleteClientToken)

	first, err := gw.adminDeleteAccount(t.Context(), body)
	require.NoError(t, err)
	started := first.(*DeleteAccountResponse)

	require.Eventually(t, func() bool {
		return describeDeletion(t, gw, account.AccountID).State == DeletionStateCompleted
	}, 60*time.Second, 100*time.Millisecond)

	second, err := gw.adminDeleteAccount(t.Context(), body)
	require.NoError(t, err)

	replayed, ok := second.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.Equal(t, started.DeletionID, replayed.DeletionID)
	assert.Equal(t, DeletionStateCompleted, replayed.State)
}

// Reusing a token for a different account name is a client bug. Treating it as
// a retry would confirm the stored name to a caller who guessed the wrong one.
func TestAdminDeleteAccountRejectsATokenBoundToAnotherName(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "d-1",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       DeletionStateRunning,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	})

	_, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "someone-else@example.com", deleteClientToken))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIdempotentParameterMismatch, err.Error())
}

// Two teardowns on one account would race the reapers against each other, so a
// live job refuses a second caller rather than joining it.
func TestAdminDeleteAccountRefusesAConcurrentTeardown(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "d-1",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       DeletionStateRunning,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	})

	_, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", strings.Repeat("b", 32)))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorOperationInProgress, err.Error())
}

// A gateway restarted mid-teardown leaves a job that looks live forever.
// Without takeover the account could never be retried through the API.
func TestAdminDeleteAccountTakesOverAStaleJob(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "d-abandoned",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       DeletionStateRunning,
		UpdatedAt:   time.Now().UTC().Add(-2 * accountDeletionStale).Format(time.RFC3339),
	})

	output, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", strings.Repeat("c", 32)))
	require.NoError(t, err)

	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.NotEqual(t, "d-abandoned", response.DeletionID, "a takeover runs under its own deletion id")
	assert.Equal(t, DeletionStateRunning, response.State)
}

// A teardown that failed is retried rather than left wedged: the engine
// re-lists what is still there, so running it again is safe.
func TestAdminDeleteAccountRetriesAFailedJob(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "d-failed",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       DeletionStateFailed,
		Error:       "volume vol-1 is stuck",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	})

	output, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", strings.Repeat("d", 32)))
	require.NoError(t, err)

	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.NotEqual(t, "d-failed", response.DeletionID)
	assert.Equal(t, DeletionStateRunning, response.State)
}

// A completed job means the account is gone. Saying so beats starting a second
// teardown that would only fail on a missing account record.
func TestAdminDeleteAccountReportsACompletedJobToANewToken(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")
	storeDeletionJob(t, gw, &accountDeletionJob{
		DeletionID:  "d-done",
		AccountID:   account.AccountID,
		AccountName: "tenant@example.com",
		ClientToken: deleteClientToken,
		State:       DeletionStateCompleted,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		FinishedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	output, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "tenant@example.com", strings.Repeat("e", 32)))
	require.NoError(t, err)

	response, ok := output.(*DeleteAccountResponse)
	require.True(t, ok)
	assert.Equal(t, "d-done", response.DeletionID)
	assert.Equal(t, DeletionStateCompleted, response.State)
}

// The protected accounts are refused the same way an unauthorized caller is:
// no credential grants deleting them, so the answer describes nothing.
func TestAdminDeleteAccountRefusesProtectedAccounts(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	newTenantAccount(t, gw, "tenant@example.com")

	// The system account and the super admin, whichever names they carry.
	for _, accountID := range []string{"000000000000", "000000000001"} {
		t.Run(accountID, func(t *testing.T) {
			_, err := gw.adminDeleteAccount(t.Context(),
				deleteAccountBody(t, accountID, "super-admin@example.com", deleteClientToken))

			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
		})
	}
}

// The name confirmation is what makes a mistyped account id fail closed rather
// than empty a live tenant, so it is checked before anything is deleted.
func TestAdminDeleteAccountRefusesAMismatchedName(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	account := newTenantAccount(t, gw, "tenant@example.com")

	_, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, account.AccountID, "wrong@example.com", deleteClientToken))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIdempotentParameterMismatch, err.Error())

	stored, err := gw.IAMService.GetAccount(account.AccountID)
	require.NoError(t, err)
	assert.Equal(t, handlers_iam.AccountStatusActive, stored.Status)
}

func TestAdminDeleteAccountRejectsAnUnknownAccount(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	newTenantAccount(t, gw, "tenant@example.com")

	_, err := gw.adminDeleteAccount(t.Context(),
		deleteAccountBody(t, "000000009999", "ghost@example.com", deleteClientToken))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())
}

func TestAdminDeleteAccountRejectsAMalformedBody(t *testing.T) {
	gw := newDeleteAccountGateway(t)

	_, err := gw.adminDeleteAccount(t.Context(), []byte(`{`))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidRequest, err.Error())
}

// Without an IAM service there is no way to check what the account owns, and
// deleting on a guess is worse than refusing.
func TestAdminDeleteAccountRefusesWithoutAnIAMService(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	_, err := gw.adminDeleteAccount(context.Background(),
		deleteAccountBody(t, "000000000042", "tenant@example.com", deleteClientToken))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestAdminDescribeAccountDeletionRejectsAMalformedBody(t *testing.T) {
	gw := newDeleteAccountGateway(t)

	_, err := gw.adminDescribeAccountDeletion(t.Context(), []byte(`{`))

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidRequest, err.Error())
}

// The stored stages are what an operator reads while a teardown runs, so the
// tracker has to write them as they finish rather than only at the end.
func TestDeletionTrackerRecordsEachStageAsItFinishes(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	job := &accountDeletionJob{
		DeletionID: "d-1",
		AccountID:  "000000000042",
		State:      DeletionStateRunning,
	}
	jobs := storeDeletionJob(t, gw, job)
	tracker := &deletionTracker{jobs: jobs, job: job}

	tracker.stageFinished(t.Context(), accountteardown.StageResult{
		Stage:   accountteardown.StageCompute,
		Deleted: []accountteardown.Resource{{Kind: "instance", ID: "i-1"}},
	})

	stored := describeDeletion(t, gw, "000000000042")
	require.Len(t, stored.Stages, 1)
	assert.Equal(t, accountteardown.StageCompute, stored.Stages[0].Stage)
	require.Len(t, stored.Stages[0].Deleted, 1)
	assert.Equal(t, "i-1", stored.Stages[0].Deleted[0].ID)
	assert.Equal(t, DeletionStateRunning, stored.State)
	assert.NotEmpty(t, stored.UpdatedAt, "a running job must heartbeat or it reads as abandoned")
}

// A teardown that gives up has to say so in the record. A job left RUNNING
// forever is indistinguishable from one still working.
func TestDeletionTrackerRecordsFailure(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	job := &accountDeletionJob{
		DeletionID: "d-1",
		AccountID:  "000000000042",
		State:      DeletionStateRunning,
	}
	jobs := storeDeletionJob(t, gw, job)
	tracker := &deletionTracker{jobs: jobs, job: job}

	tracker.finished(t.Context(), nil, assert.AnError)

	stored := describeDeletion(t, gw, "000000000042")
	assert.Equal(t, DeletionStateFailed, stored.State)
	assert.Contains(t, stored.Error, assert.AnError.Error())
	assert.NotEmpty(t, stored.FinishedAt)
}

// The heartbeat is what lets another gateway tell an abandoned job from a live
// one, so stopping it must not leave the goroutine writing.
func TestDeletionTrackerHeartbeatStops(t *testing.T) {
	gw := newDeleteAccountGateway(t)
	job := &accountDeletionJob{
		DeletionID: "d-1",
		AccountID:  "000000000042",
		State:      DeletionStateRunning,
	}
	jobs := storeDeletionJob(t, gw, job)
	tracker := &deletionTracker{jobs: jobs, job: job}

	stop := tracker.startHeartbeat(t.Context())
	stop()

	tracker.persist(t.Context())
	assert.NotEmpty(t, describeDeletion(t, gw, "000000000042").UpdatedAt)
}
