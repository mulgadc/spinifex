package account

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupFakeKV wraps a real KeyValue so a test can inject a race between
// CleanupAccount's read and its guarded delete, or a store failure,
// deterministically rather than through goroutine timing.
type cleanupFakeKV struct {
	jetstream.KeyValue

	deleteCalls  int
	beforeDelete func(attempt int) error
	getErr       error
}

func (f *cleanupFakeKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.KeyValue.Get(ctx, key)
}

func (f *cleanupFakeKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	f.deleteCalls++
	if f.beforeDelete != nil {
		if err := f.beforeDelete(f.deleteCalls); err != nil {
			return err
		}
	}
	return f.KeyValue.Delete(ctx, key, opts...)
}

func TestCleanupAccount_DeletesTheSettingsKey(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

	result, err := svc.CleanupAccount(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsCleanupDeleted, result.Disposition)
	assert.Equal(t, evidenceRevisionGuardedDelete, result.Evidence)
	assert.NotZero(t, result.Revision)

	_, err = svc.settingsKV.Get(ctx, testAccountID)
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

func TestCleanupAccount_RepeatCallIsIdempotent(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

	first, err := svc.CleanupAccount(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsCleanupDeleted, first.Disposition)

	second, err := svc.CleanupAccount(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsCleanupAbsent, second.Disposition)
	assert.Equal(t, evidenceObservedAbsent, second.Evidence)
}

func TestCleanupAccount_AbsentKeyIsIdempotent(t *testing.T) {
	svc := setupTestAccountService(t)
	result, err := svc.CleanupAccount(context.Background(), "333333333333")
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsCleanupAbsent, result.Disposition)
	assert.Equal(t, evidenceObservedAbsent, result.Evidence)
}

func TestCleanupAccount_OtherAccountsUntouched(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	accountA, accountB := testAccountID, "222222222222"
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, accountA)
	require.NoError(t, err)
	_, err = svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, accountB)
	require.NoError(t, err)

	result, err := svc.CleanupAccount(ctx, accountA)
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsCleanupDeleted, result.Disposition)

	out, err := svc.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{}, accountB)
	require.NoError(t, err)
	assert.True(t, *out.EbsEncryptionByDefault)
}

func TestCleanupAccount_RefusesGlobalAccountID(t *testing.T) {
	svc := setupTestAccountService(t)
	result, err := svc.CleanupAccount(context.Background(), awsidentifiers.GlobalAccountID)
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestCleanupAccount_RefusesEmptyAccountID(t *testing.T) {
	svc := setupTestAccountService(t)
	result, err := svc.CleanupAccount(context.Background(), "")
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestCleanupAccount_RefusesInvalidAccountID(t *testing.T) {
	svc := setupTestAccountService(t)
	result, err := svc.CleanupAccount(context.Background(), "not-an-account")
	assert.Error(t, err)
	assert.Nil(t, result)
}

// A concurrent Put landing between CleanupAccount's read and its
// revision-guarded delete must not be lost silently: the delete is rejected
// and retried against the revision the write left behind.
func TestCleanupAccount_ConcurrentPutDuringCleanupRetriesAndDeletes(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

	fake := &cleanupFakeKV{KeyValue: svc.settingsKV}
	fake.beforeDelete = func(attempt int) error {
		if attempt != 1 {
			return nil
		}
		// Simulate a racing settings write landing after CleanupAccount's
		// read but before its guarded delete, through the real KV so the
		// delete's expected revision is now stale.
		return svc.saveSettings(ctx, &AccountSettingsRecord{SerialConsoleAccess: true}, testAccountID)
	}
	svc.settingsKV = fake

	result, err := svc.CleanupAccount(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsCleanupDeleted, result.Disposition)
	assert.Equal(t, 2, fake.deleteCalls, "the first delete is rejected by the race and retried once")
}

// A revision that keeps moving under sustained concurrent writes must not
// spin forever: cleanup gives up and reports failed, bounded by attempts.
func TestCleanupAccount_PersistentConcurrentWritesFailsBounded(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

	fake := &cleanupFakeKV{KeyValue: svc.settingsKV}
	fake.beforeDelete = func(int) error {
		return svc.saveSettings(ctx, &AccountSettingsRecord{SerialConsoleAccess: true}, testAccountID)
	}
	svc.settingsKV = fake

	result, err := svc.CleanupAccount(ctx, testAccountID)
	assert.Error(t, err)
	require.NotNil(t, result)
	assert.Equal(t, AccountSettingsCleanupFailed, result.Disposition)
	assert.Equal(t, cleanupMaxAttempts, fake.deleteCalls)
}

// A store error must report failed, never absent: absence is a claim about
// what the store holds, which an error gives no basis to make.
func TestCleanupAccount_StoreErrorIsFailedNotAbsent(t *testing.T) {
	svc := setupTestAccountService(t)
	fake := &cleanupFakeKV{KeyValue: svc.settingsKV, getErr: errors.New("kv unavailable")}
	svc.settingsKV = fake

	result, err := svc.CleanupAccount(context.Background(), testAccountID)
	assert.Error(t, err)
	require.NotNil(t, result)
	assert.Equal(t, AccountSettingsCleanupFailed, result.Disposition)
}
