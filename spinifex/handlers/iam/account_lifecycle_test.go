package handlers_iam_test

import (
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLifecycleService(t *testing.T) handlers_iam.IAMService {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	svc, err := handlers_iam.NewIAMServiceImpl(t.Context(), nc, masterKey, 1)
	require.NoError(t, err)
	return svc
}

func TestSetAccountStatusMovesBetweenStates(t *testing.T) {
	svc := newLifecycleService(t)
	account, err := svc.CreateAccount("tenant@example.com")
	require.NoError(t, err)

	suspended, err := svc.SetAccountStatus(account.AccountID, handlers_iam.AccountStatusSuspended)
	require.NoError(t, err)
	assert.Equal(t, handlers_iam.AccountStatusSuspended, suspended.Status)

	// An operator hold is reversible, unlike a teardown.
	restored, err := svc.SetAccountStatus(account.AccountID, handlers_iam.AccountStatusActive)
	require.NoError(t, err)
	assert.Equal(t, handlers_iam.AccountStatusActive, restored.Status)

	stored, err := svc.GetAccount(account.AccountID)
	require.NoError(t, err)
	assert.Equal(t, handlers_iam.AccountStatusActive, stored.Status)
}

// There is no path back from TERMINATING. A half-torn-down account that
// resumes service is worse than one that stays down, and "undo" here would
// mean restoring deleted block storage.
func TestTerminatingIsAOneWayDoor(t *testing.T) {
	svc := newLifecycleService(t)
	account, err := svc.CreateAccount("tenant@example.com")
	require.NoError(t, err)

	_, err = svc.SetAccountStatus(account.AccountID, handlers_iam.AccountStatusTerminating)
	require.NoError(t, err)

	for _, status := range []string{handlers_iam.AccountStatusActive, handlers_iam.AccountStatusSuspended} {
		_, err = svc.SetAccountStatus(account.AccountID, status)
		require.Error(t, err)
	}

	stored, err := svc.GetAccount(account.AccountID)
	require.NoError(t, err)
	assert.Equal(t, handlers_iam.AccountStatusTerminating, stored.Status)
}

func TestSetAccountStatusRejectsAnUnknownStatus(t *testing.T) {
	svc := newLifecycleService(t)
	account, err := svc.CreateAccount("tenant@example.com")
	require.NoError(t, err)

	_, err = svc.SetAccountStatus(account.AccountID, "PAUSED")

	assert.Error(t, err)
}

// Deleting the record while the account is still ACTIVE would leave every
// resource it owns unattributable, so the status gate is the invariant.
func TestDeleteAccountRequiresTerminating(t *testing.T) {
	svc := newLifecycleService(t)
	// Account ids are sequential and the first is the super admin, which is
	// protected — the tenant under test has to be the second.
	_, err := svc.CreateAccount("super-admin@example.com")
	require.NoError(t, err)
	account, err := svc.CreateAccount("tenant@example.com")
	require.NoError(t, err)

	require.Error(t, svc.DeleteAccount(account.AccountID))

	_, err = svc.SetAccountStatus(account.AccountID, handlers_iam.AccountStatusTerminating)
	require.NoError(t, err)
	require.NoError(t, svc.DeleteAccount(account.AccountID))

	_, err = svc.GetAccount(account.AccountID)
	assert.Error(t, err)
}

// No credential grants deleting these two, so the refusal lives in the library
// rather than only in the CLI that usually calls it.
func TestProtectedAccountsCanNeverBeDeleted(t *testing.T) {
	svc := newLifecycleService(t)

	for accountID := range handlers_iam.UndeletableAccountIDs {
		t.Run(accountID, func(t *testing.T) {
			// Even a status that would otherwise permit deletion must not.
			_, _ = svc.SetAccountStatus(accountID, handlers_iam.AccountStatusTerminating)

			err := svc.DeleteAccount(accountID)

			require.Error(t, err)
			assert.ErrorIs(t, err, handlers_iam.ErrAccountUndeletable)
		})
	}
}

// A deleted account's name must be free again: a customer who leaves and comes
// back must not be told their own address is taken.
func TestReleaseDeletedFreesTheName(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	ctx := t.Context()

	require.NoError(t, index.Reserve(ctx, "tenant@example.com", "token-1"))
	require.NoError(t, index.Commit(ctx, "tenant@example.com", "000000000042", "token-1"))

	require.NoError(t, index.ReleaseDeleted(ctx, "tenant@example.com", "000000000042"))

	_, found, err := index.Lookup(ctx, "tenant@example.com")
	require.NoError(t, err)
	assert.False(t, found)

	// The same address can be claimed again, by a different account.
	require.NoError(t, index.Reserve(ctx, "tenant@example.com", "token-2"))
}

// Releasing a name that belongs to a live account would unindex it and let a
// second account take the same address.
func TestReleaseDeletedRefusesAnotherAccountsName(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	ctx := t.Context()

	require.NoError(t, index.Reserve(ctx, "tenant@example.com", "token-1"))
	require.NoError(t, index.Commit(ctx, "tenant@example.com", "000000000099", "token-1"))

	require.Error(t, index.ReleaseDeleted(ctx, "tenant@example.com", "000000000042"))

	accountID, found, err := index.Lookup(ctx, "tenant@example.com")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "000000000099", accountID)
}

// Teardown re-runs after a crash, so a name that is already released is a
// success rather than a failure that blocks the account record's deletion.
func TestReleaseDeletedIsIdempotent(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	ctx := t.Context()

	assert.NoError(t, index.ReleaseDeleted(ctx, "never-reserved@example.com", "000000000042"))
}
