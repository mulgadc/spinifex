package iam

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetAccountStatusCountsOnlyChanges(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	account, err := svc.CreateAccount("generation-counts")
	require.NoError(t, err)
	assert.Zero(t, account.Generation)

	steps := []struct {
		status string
		want   uint64
	}{
		{AccountStatusActive, 0},
		{AccountStatusSuspended, 1},
		{AccountStatusSuspended, 1},
		{AccountStatusActive, 2},
		{AccountStatusTerminating, 3},
		{AccountStatusTerminating, 3},
	}
	for _, step := range steps {
		got, err := svc.SetAccountStatus(account.AccountID, step.status)
		require.NoError(t, err)
		assert.Equal(t, step.want, got.Generation, "status %s", step.status)

		stored, err := svc.GetAccount(account.AccountID)
		require.NoError(t, err)
		assert.Equal(t, step.want, stored.Generation, "stored record after %s", step.status)
	}
}

func TestAccountRecordWithoutGenerationReadsAsZero(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	account, err := svc.CreateAccount("legacy-record")
	require.NoError(t, err)

	legacy, err := json.Marshal(map[string]string{
		"account_id": account.AccountID, "account_name": "legacy-record",
		"status": AccountStatusActive, "created_at": account.CreatedAt,
	})
	require.NoError(t, err)
	_, err = svc.accountsBucket.Put(context.Background(), account.AccountID, legacy)
	require.NoError(t, err)

	got, err := svc.SetAccountStatus(account.AccountID, AccountStatusSuspended)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), got.Generation)
}

// racingBucket lets a second writer land between the status writer's read and its update.
type racingBucket struct {
	jetstream.KeyValue

	raced   atomic.Bool
	compete func()
}

func (b *racingBucket) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if b.raced.CompareAndSwap(false, true) {
		b.compete()
	}
	return b.KeyValue.Update(ctx, key, value, last)
}

func TestSetAccountStatusRetriesOnAConcurrentWrite(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	account, err := svc.CreateAccount("racing-writer")
	require.NoError(t, err)

	underlying := svc.accountsBucket
	svc.accountsBucket = &racingBucket{KeyValue: underlying, compete: func() {
		entry, err := underlying.Get(context.Background(), account.AccountID)
		require.NoError(t, err)
		var other Account
		require.NoError(t, json.Unmarshal(entry.Value(), &other))
		other.AccountName = "renamed-by-the-other-writer"
		data, err := json.Marshal(other)
		require.NoError(t, err)
		_, err = underlying.Put(context.Background(), account.AccountID, data)
		require.NoError(t, err)
	}}

	got, err := svc.SetAccountStatus(account.AccountID, AccountStatusSuspended)
	require.NoError(t, err)

	stored, err := svc.GetAccount(account.AccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountStatusSuspended, stored.Status, "the status change must land")
	assert.Equal(t, "renamed-by-the-other-writer", stored.AccountName, "the competing write must not be lost")
	assert.Equal(t, uint64(1), stored.Generation)
	assert.Equal(t, stored.Generation, got.Generation)
}
