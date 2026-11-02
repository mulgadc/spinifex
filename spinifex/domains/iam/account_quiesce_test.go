package iam

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newQuiesceAccount returns a service and a tenant account. The first account a
// fresh service creates takes the protected admin ID, so it is burned first.
func newQuiesceAccount(t *testing.T) (*IAMServiceImpl, string) {
	t.Helper()
	svc := setupTestIAMService(t)
	_, err := svc.CreateAccount("reserved-admin")
	require.NoError(t, err)
	account, err := svc.CreateAccount("quiesce-target")
	require.NoError(t, err)
	return svc, account.AccountID
}

// writeAccountRecord rewrites the stored record directly, to produce states the
// service itself would not, such as a record written by an older binary.
func writeAccountRecord(t *testing.T, svc *IAMServiceImpl, id string, mutate func(*Account)) {
	t.Helper()
	account, err := svc.GetAccount(id)
	require.NoError(t, err)
	mutate(account)
	raw, err := json.Marshal(account)
	require.NoError(t, err)
	_, err = svc.accountsBucket.Put(context.Background(), id, raw)
	require.NoError(t, err)
}

func TestQuiesceAccountRuleTable(t *testing.T) {
	t.Parallel()

	type op struct {
		id    string
		fence uint64
	}
	cases := []struct {
		name string
		// start puts the account in its starting state.
		start func(t *testing.T, svc *IAMServiceImpl, id string)
		call  op
		// wantErr is nil when the call must succeed.
		wantErr error
		// wantGeneration is the generation returned and stored.
		wantGeneration uint64
		wantStatus     string
		wantOp         *TerminationOp
	}{
		{
			name:           "active becomes terminating and records the operation",
			start:          func(*testing.T, *IAMServiceImpl, string) {},
			call:           op{"op-1", 1},
			wantGeneration: 1,
			wantStatus:     AccountStatusTerminating,
			wantOp:         &TerminationOp{OperationID: "op-1", Fence: 1},
		},
		{
			name: "suspended becomes terminating and records the operation",
			start: func(t *testing.T, svc *IAMServiceImpl, id string) {
				_, err := svc.SetAccountStatus(id, AccountStatusSuspended)
				require.NoError(t, err)
			},
			call:           op{"op-1", 1},
			wantGeneration: 2,
			wantStatus:     AccountStatusTerminating,
			wantOp:         &TerminationOp{OperationID: "op-1", Fence: 1},
		},
		{
			name: "same operation and fence is idempotent",
			start: func(t *testing.T, svc *IAMServiceImpl, id string) {
				_, err := svc.QuiesceAccount(id, "op-1", 1)
				require.NoError(t, err)
			},
			call:           op{"op-1", 1},
			wantGeneration: 1,
			wantStatus:     AccountStatusTerminating,
			wantOp:         &TerminationOp{OperationID: "op-1", Fence: 1},
		},
		{
			name: "same operation at a higher fence records the fence only",
			start: func(t *testing.T, svc *IAMServiceImpl, id string) {
				_, err := svc.QuiesceAccount(id, "op-1", 1)
				require.NoError(t, err)
			},
			call:           op{"op-1", 2},
			wantGeneration: 1,
			wantStatus:     AccountStatusTerminating,
			wantOp:         &TerminationOp{OperationID: "op-1", Fence: 2},
		},
		{
			name: "same operation at a lower fence is refused",
			start: func(t *testing.T, svc *IAMServiceImpl, id string) {
				_, err := svc.QuiesceAccount(id, "op-1", 3)
				require.NoError(t, err)
			},
			call:       op{"op-1", 2},
			wantErr:    ErrQuiesceSuperseded,
			wantStatus: AccountStatusTerminating,
			wantOp:     &TerminationOp{OperationID: "op-1", Fence: 3},
		},
		{
			name: "a different operation is refused even at a higher fence",
			start: func(t *testing.T, svc *IAMServiceImpl, id string) {
				_, err := svc.QuiesceAccount(id, "op-1", 1)
				require.NoError(t, err)
			},
			call:       op{"op-2", 9},
			wantErr:    ErrQuiesceSuperseded,
			wantStatus: AccountStatusTerminating,
			wantOp:     &TerminationOp{OperationID: "op-1", Fence: 1},
		},
		{
			name: "terminating without an operation, written by an older binary, adopts it",
			start: func(t *testing.T, svc *IAMServiceImpl, id string) {
				writeAccountRecord(t, svc, id, func(a *Account) {
					a.Status = AccountStatusTerminating
					a.Generation = 4
				})
			},
			call:           op{"op-1", 1},
			wantGeneration: 4,
			wantStatus:     AccountStatusTerminating,
			wantOp:         &TerminationOp{OperationID: "op-1", Fence: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, id := newQuiesceAccount(t)
			tc.start(t, svc, id)

			generation, err := svc.QuiesceAccount(id, tc.call.id, tc.call.fence)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantGeneration, generation)
			}

			stored, err := svc.GetAccount(id)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, stored.Status)
			assert.Equal(t, tc.wantOp, stored.TerminationOp)
			if tc.wantErr == nil {
				assert.Equal(t, tc.wantGeneration, stored.Generation)
			}
		})
	}
}

func TestQuiesceAccountRefusesTheProtectedAccounts(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	systemSecret, err := svc.key.EncryptBase64("system-secret")
	require.NoError(t, err)
	adminSecret, err := svc.key.EncryptBase64("admin-secret")
	require.NoError(t, err)
	require.NoError(t, svc.SeedBootstrap(&BootstrapData{
		AccessKeyID:     "AKIASYSTEM1234567890",
		EncryptedSecret: systemSecret,
		AccountID:       awsidentifiers.GlobalAccountID,
		Admin: &AdminBootstrapData{
			AccountID:       "000000000001",
			AccountName:     "spinifex",
			UserName:        "admin",
			AccessKeyID:     "AKIAADMIN12345678901",
			EncryptedSecret: adminSecret,
		},
	}))

	for _, id := range []string{awsidentifiers.GlobalAccountID, "000000000001"} {
		generation, err := svc.QuiesceAccount(id, "op-1", 1)
		require.ErrorIs(t, err, ErrAccountUndeletable, id)
		assert.Zero(t, generation)
	}

	stored, err := svc.GetAccount(awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountStatusActive, stored.Status)
	assert.Nil(t, stored.TerminationOp)
}

func TestQuiesceAccountNeedsAnOperationAndAFence(t *testing.T) {
	t.Parallel()
	svc, id := newQuiesceAccount(t)

	for _, call := range []struct {
		op    string
		fence uint64
	}{{"", 1}, {"op-1", 0}} {
		_, err := svc.QuiesceAccount(id, call.op, call.fence)
		require.Error(t, err)
	}

	stored, err := svc.GetAccount(id)
	require.NoError(t, err)
	assert.Equal(t, AccountStatusActive, stored.Status)
}

func TestQuiesceAccountOfAMissingAccountIsNoSuchEntity(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	_, err := svc.QuiesceAccount("000000000077", "op-1", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NoSuchEntity")
}

// Entering TERMINATING without an owning operation would leave an account that
// no job can be fenced against, so SetAccountStatus refuses it.
func TestSetAccountStatusRefusesTerminating(t *testing.T) {
	t.Parallel()
	svc, id := newQuiesceAccount(t)

	got, err := svc.SetAccountStatus(id, AccountStatusTerminating)
	require.ErrorIs(t, err, ErrUseQuiesceAccount)
	assert.Nil(t, got)

	stored, err := svc.GetAccount(id)
	require.NoError(t, err)
	assert.Equal(t, AccountStatusActive, stored.Status)
	assert.Zero(t, stored.Generation)
}

// A status write by SetAccountStatus must not drop the operation record.
func TestSetAccountStatusKeepsTheTerminationOp(t *testing.T) {
	t.Parallel()
	svc, id := newQuiesceAccount(t)
	_, err := svc.QuiesceAccount(id, "op-1", 2)
	require.NoError(t, err)

	_, err = svc.SetAccountStatus(id, AccountStatusSuspended)
	require.ErrorIs(t, err, ErrAccountUndeletable)

	stored, err := svc.GetAccount(id)
	require.NoError(t, err)
	assert.Equal(t, &TerminationOp{OperationID: "op-1", Fence: 2}, stored.TerminationOp)
}

// racingDeleteBucket lets a takeover land between the delete's read and its
// conditional delete.
type racingDeleteBucket struct {
	jetstream.KeyValue

	raced   atomic.Bool
	compete func()
}

func (b *racingDeleteBucket) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if b.raced.CompareAndSwap(false, true) {
		b.compete()
	}
	return b.KeyValue.Delete(ctx, key, opts...)
}

// The final removal is fenced: only the holder whose operation and fence are
// on the record may remove the account.
func TestDeleteAccountIsFenced(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		op      string
		fence   uint64
		wantErr error
	}{
		{"the matching operation and fence removes the account", "op-1", 2, nil},
		{"a stale fence cannot remove the account", "op-1", 1, ErrQuiesceSuperseded},
		{"a newer fence than the record cannot remove the account", "op-1", 3, ErrQuiesceSuperseded},
		{"a different operation cannot remove the account", "op-2", 2, ErrQuiesceSuperseded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, id := newQuiesceAccount(t)
			_, err := svc.QuiesceAccount(id, "op-1", 2)
			require.NoError(t, err)

			err = svc.DeleteAccount(id, tc.op, tc.fence)

			_, getErr := svc.GetAccount(id)
			if tc.wantErr == nil {
				require.NoError(t, err)
				require.Error(t, getErr, "the account must be gone")
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			require.NoError(t, getErr, "a refused delete must remove nothing")
		})
	}
}

func TestDeleteAccountOfARecordWithoutAnOperationIsRefused(t *testing.T) {
	t.Parallel()
	svc, id := newQuiesceAccount(t)
	writeAccountRecord(t, svc, id, func(a *Account) { a.Status = AccountStatusTerminating })

	require.ErrorIs(t, svc.DeleteAccount(id, "op-1", 1), ErrQuiesceSuperseded)
	_, err := svc.GetAccount(id)
	require.NoError(t, err)
}

// A record that changes between the read and the delete is not removed on the
// strength of the earlier read: the conditional delete refuses and the retry
// re-reads, so a takeover in that window wins.
func TestDeleteAccountDoesNotRemoveARecordTakenOverMidDelete(t *testing.T) {
	t.Parallel()
	svc, id := newQuiesceAccount(t)
	_, err := svc.QuiesceAccount(id, "op-1", 1)
	require.NoError(t, err)

	underlying := svc.accountsBucket
	svc.accountsBucket = &racingDeleteBucket{KeyValue: underlying, compete: func() {
		_, takeoverErr := svc.QuiesceAccount(id, "op-1", 2)
		require.NoError(t, takeoverErr)
	}}

	require.ErrorIs(t, svc.DeleteAccount(id, "op-1", 1), ErrQuiesceSuperseded)
	svc.accountsBucket = underlying
	stored, err := svc.GetAccount(id)
	require.NoError(t, err)
	assert.Equal(t, &TerminationOp{OperationID: "op-1", Fence: 2}, stored.TerminationOp)
}
