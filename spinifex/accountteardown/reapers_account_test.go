package accountteardown

//test:in-package — the final-stage reapers are unexported, and what they
// guarantee (the name is released, and only to its own account) is only
// observable from inside the package.

import (
	"context"
	"errors"
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountIAM adds the account-record calls the final stage needs to the IAM
// double the identity reapers use.
type accountIAM struct {
	fakeIAM

	account   *handlers_iam.Account
	getErr    error
	deleted   bool
	setStatus string
}

func (f *accountIAM) GetAccount(string) (*handlers_iam.Account, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.account, nil
}

func (f *accountIAM) SetAccountStatus(_, status string) (*handlers_iam.Account, error) {
	f.setStatus = status
	updated := *f.account
	updated.Status = status
	f.account = &updated
	return f.account, nil
}

func (f *accountIAM) DeleteAccount(string) error {
	f.deleted = true
	return nil
}

func testKV(t *testing.T, bucket string) (context.Context, jetstream.JetStream, jetstream.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	ctx := testCtx(t)
	kv, err := kvutil.GetOrCreateBucket(ctx, js, bucket, 1)
	require.NoError(t, err)
	return ctx, js, kv
}

// Left behind, the counter is a small leak that also makes a deleted account
// id look live to anything reading the bucket to decide what exists.
func TestQuotaUsageReaperRemovesTheCounter(t *testing.T) {
	ctx, _, usage := testKV(t, "spinifex-account-usage-test")
	_, err := usage.Put(ctx, "000000000042", []byte(`{"vcpus":4}`))
	require.NoError(t, err)

	reaper := &quotaUsageReaper{usage: usage}
	assert.Equal(t, StageAccount, reaper.Stage())

	found, err := reaper.List(ctx, "000000000042")
	require.NoError(t, err)
	require.Len(t, found, 1)

	require.NoError(t, reaper.Delete(ctx, "000000000042", found[0], false))

	found, err = reaper.List(ctx, "000000000042")
	require.NoError(t, err)
	assert.Empty(t, found)
}

// A cluster that never enabled quotas has no counter to remove, which is not a
// reason to refuse a teardown.
func TestQuotaUsageReaperToleratesAnAbsentBucket(t *testing.T) {
	ctx, _, usage := testKV(t, "spinifex-account-usage-absent")

	reaper := &quotaUsageReaper{usage: usage}
	found, err := reaper.List(ctx, "000000000042")
	require.NoError(t, err)
	assert.Empty(t, found)
	assert.NoError(t, reaper.Delete(ctx, "000000000042", Resource{ID: "000000000042"}, false))

	nilReaper := &quotaUsageReaper{}
	found, err = nilReaper.List(ctx, "000000000042")
	require.NoError(t, err)
	assert.Empty(t, found)
	assert.NoError(t, nilReaper.Delete(ctx, "000000000042", Resource{ID: "000000000042"}, false))
}

// A customer who deletes an account and comes back must not be told their own
// address is taken by a tenant that no longer exists.
func TestNameReservationReaperReleasesTheName(t *testing.T) {
	_, js, _ := testKV(t, "spinifex-name-reaper-warmup")
	ctx := testCtx(t)

	names, err := handlers_iam.NewAccountNameIndex(ctx, js)
	require.NoError(t, err)
	require.NoError(t, names.Reserve(ctx, "tenant@example.com", "token-1"))
	require.NoError(t, names.Commit(ctx, "tenant@example.com", "000000000042", "token-1"))

	svc := &accountIAM{account: &handlers_iam.Account{
		AccountID: "000000000042", AccountName: "tenant@example.com",
	}}
	reaper := &nameReservationReaper{svc: svc, names: names}

	found, err := reaper.List(ctx, "000000000042")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "tenant@example.com", found[0].ID)

	require.NoError(t, reaper.Delete(ctx, "000000000042", found[0], false))

	_, stillReserved, err := names.Lookup(ctx, "tenant@example.com")
	require.NoError(t, err)
	assert.False(t, stillReserved)
}

// A reservation held by a different account is not this teardown's to release:
// a stale name must not take another tenant's reservation with it.
func TestNameReservationReaperIgnoresAnotherAccountsName(t *testing.T) {
	_, js, _ := testKV(t, "spinifex-name-reaper-other")
	ctx := testCtx(t)

	names, err := handlers_iam.NewAccountNameIndex(ctx, js)
	require.NoError(t, err)
	require.NoError(t, names.Reserve(ctx, "tenant@example.com", "token-1"))
	require.NoError(t, names.Commit(ctx, "tenant@example.com", "000000000099", "token-1"))

	svc := &accountIAM{account: &handlers_iam.Account{
		AccountID: "000000000042", AccountName: "tenant@example.com",
	}}

	found, err := (&nameReservationReaper{svc: svc, names: names}).List(ctx, "000000000042")
	require.NoError(t, err)
	assert.Empty(t, found)

	_, stillReserved, err := names.Lookup(ctx, "tenant@example.com")
	require.NoError(t, err)
	assert.True(t, stillReserved)
}

// The account record is deleted after this stage, so its absence means someone
// else removed it and the reservation can no longer be attributed.
func TestNameReservationReaperReportsAMissingAccount(t *testing.T) {
	_, js, _ := testKV(t, "spinifex-name-reaper-missing")
	ctx := testCtx(t)

	names, err := handlers_iam.NewAccountNameIndex(ctx, js)
	require.NoError(t, err)

	svc := &accountIAM{getErr: errors.New("account not found")}
	_, err = (&nameReservationReaper{svc: svc, names: names}).List(ctx, "000000000042")

	assert.Error(t, err)
}

func TestAccountReapersRunLast(t *testing.T) {
	reapers := AccountReapers(&accountIAM{}, nil, nil)

	var kinds []string
	for _, reaper := range reapers {
		kinds = append(kinds, reaper.Kind())
		assert.Equal(t, StageAccount, reaper.Stage())
	}
	assert.Equal(t, []string{"quota-usage", "name-reservation"}, kinds)
}

// The adapter is the one place the engine's Account and the IAM record meet,
// so a field dropped in translation would silently break the name confirmation.
func TestIAMAccountsAdapterCarriesEveryField(t *testing.T) {
	svc := &accountIAM{account: &handlers_iam.Account{
		AccountID:   "000000000042",
		AccountName: "tenant@example.com",
		Status:      handlers_iam.AccountStatusActive,
	}}
	accounts := NewIAMAccounts(svc)

	account, err := accounts.GetAccount("000000000042")
	require.NoError(t, err)
	assert.Equal(t, "000000000042", account.AccountID)
	assert.Equal(t, "tenant@example.com", account.AccountName)
	assert.Equal(t, handlers_iam.AccountStatusActive, account.Status)

	updated, err := accounts.SetAccountStatus("000000000042", AccountStatusTerminating)
	require.NoError(t, err)
	assert.Equal(t, AccountStatusTerminating, updated.Status)
	assert.Equal(t, handlers_iam.AccountStatusTerminating, svc.setStatus)

	require.NoError(t, accounts.DeleteAccount("000000000042"))
	assert.True(t, svc.deleted)
}

// The engine declares TERMINATING itself so its tests need no IAM service. The
// two must not drift: a mismatch would mark accounts with a status the auth
// gate does not recognise, and they would keep authenticating.
func TestTerminatingStatusMatchesTheIAMRecord(t *testing.T) {
	assert.Equal(t, handlers_iam.AccountStatusTerminating, AccountStatusTerminating)
}
