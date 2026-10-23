//test:in-package — CleanupAccount deletes through the unexported overrides field, and the retry/race behaviour is only observable from inside the package.
package quota

import (
	"context"
	"errors"
	"testing"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// overrideCleanupFakeKV wraps a real KeyValue so a test can inject a race
// between CleanupAccount's read and its guarded delete, or a store failure,
// deterministically rather than through goroutine timing.
type overrideCleanupFakeKV struct {
	jetstream.KeyValue

	deleteCalls  int
	beforeDelete func(attempt int) error
	getErr       error
}

func (f *overrideCleanupFakeKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.KeyValue.Get(ctx, key)
}

func (f *overrideCleanupFakeKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	f.deleteCalls++
	if f.beforeDelete != nil {
		if err := f.beforeDelete(f.deleteCalls); err != nil {
			return err
		}
	}
	return f.KeyValue.Delete(ctx, key, opts...)
}

func TestCleanupAccount_DeletesTheOverrideKey(t *testing.T) {
	svc, _ := newOverrideService(t)
	ctx := t.Context()
	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(32)}, "operator"))

	result, err := svc.CleanupAccount(ctx, testAccount)
	require.NoError(t, err)
	assert.Equal(t, OverrideCleanupDeleted, result.Disposition)
	assert.Equal(t, evidenceRevisionGuardedDelete, result.Evidence)
	assert.NotZero(t, result.Revision)

	_, err = svc.overrides.Get(ctx, testAccount)
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

func TestCleanupAccount_RepeatCallIsIdempotent(t *testing.T) {
	svc, _ := newOverrideService(t)
	ctx := t.Context()
	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(32)}, "operator"))

	first, err := svc.CleanupAccount(ctx, testAccount)
	require.NoError(t, err)
	assert.Equal(t, OverrideCleanupDeleted, first.Disposition)

	second, err := svc.CleanupAccount(ctx, testAccount)
	require.NoError(t, err)
	assert.Equal(t, OverrideCleanupAbsent, second.Disposition)
	assert.Equal(t, evidenceObservedAbsent, second.Evidence)
}

func TestCleanupAccount_AbsentKeyIsIdempotent(t *testing.T) {
	svc, _ := newOverrideService(t)
	result, err := svc.CleanupAccount(t.Context(), "333333333333")
	require.NoError(t, err)
	assert.Equal(t, OverrideCleanupAbsent, result.Disposition)
	assert.Equal(t, evidenceObservedAbsent, result.Evidence)
}

func TestCleanupAccount_OtherAccountsUntouched(t *testing.T) {
	svc, _ := newOverrideService(t)
	ctx := t.Context()
	accountA, accountB := testAccount, "222222222222"
	require.NoError(t, svc.PutAccountQuota(ctx, accountA, Overrides{VCPUs: ptr(32)}, "operator"))
	require.NoError(t, svc.PutAccountQuota(ctx, accountB, Overrides{VCPUs: ptr(8)}, "operator"))

	result, err := svc.CleanupAccount(ctx, accountA)
	require.NoError(t, err)
	assert.Equal(t, OverrideCleanupDeleted, result.Disposition)

	over, _, err := svc.GetAccountQuota(ctx, accountB)
	require.NoError(t, err)
	require.NotNil(t, over.VCPUs)
	assert.Equal(t, 8, *over.VCPUs)
}

func TestCleanupAccount_RefusesGlobalAccountID(t *testing.T) {
	svc, _ := newOverrideService(t)
	result, err := svc.CleanupAccount(t.Context(), awsidentifiers.GlobalAccountID)
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestCleanupAccount_RefusesEmptyAccountID(t *testing.T) {
	svc, _ := newOverrideService(t)
	result, err := svc.CleanupAccount(t.Context(), "")
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestCleanupAccount_RefusesInvalidAccountID(t *testing.T) {
	svc, _ := newOverrideService(t)
	result, err := svc.CleanupAccount(t.Context(), "not-an-account")
	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestCleanupAccount_WithoutBucketFails(t *testing.T) {
	svc := New(baseLimits(), nil)
	result, err := svc.CleanupAccount(t.Context(), testAccount)
	assert.Error(t, err)
	require.NotNil(t, result)
	assert.Equal(t, OverrideCleanupFailed, result.Disposition)
}

// A concurrent Put landing between CleanupAccount's read and its
// revision-guarded delete must not be lost silently: the delete is rejected
// and retried against the revision the write left behind.
func TestCleanupAccount_ConcurrentPutDuringCleanupRetriesAndDeletes(t *testing.T) {
	svc, _ := newOverrideService(t)
	ctx := t.Context()
	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(32)}, "operator"))

	fake := &overrideCleanupFakeKV{KeyValue: svc.overrides}
	fake.beforeDelete = func(attempt int) error {
		if attempt != 1 {
			return nil
		}
		// Simulate a racing override write landing after CleanupAccount's
		// read but before its guarded delete, through the real KV so the
		// delete's expected revision is now stale.
		return svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator")
	}
	svc.overrides = fake

	result, err := svc.CleanupAccount(ctx, testAccount)
	require.NoError(t, err)
	assert.Equal(t, OverrideCleanupDeleted, result.Disposition)
	assert.Equal(t, 2, fake.deleteCalls, "the first delete is rejected by the race and retried once")
}

// A revision that keeps moving under sustained concurrent writes must not
// spin forever: cleanup gives up and reports failed, bounded by attempts.
func TestCleanupAccount_PersistentConcurrentWritesFailsBounded(t *testing.T) {
	svc, _ := newOverrideService(t)
	ctx := t.Context()
	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(32)}, "operator"))

	fake := &overrideCleanupFakeKV{KeyValue: svc.overrides}
	fake.beforeDelete = func(int) error {
		return svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator")
	}
	svc.overrides = fake

	result, err := svc.CleanupAccount(ctx, testAccount)
	assert.Error(t, err)
	require.NotNil(t, result)
	assert.Equal(t, OverrideCleanupFailed, result.Disposition)
	assert.Equal(t, cleanupMaxAttempts, fake.deleteCalls)
}

// A store error must report failed, never absent: absence is a claim about
// what the store holds, which an error gives no basis to make.
func TestCleanupAccount_StoreErrorIsFailedNotAbsent(t *testing.T) {
	svc, _ := newOverrideService(t)
	fake := &overrideCleanupFakeKV{KeyValue: svc.overrides, getErr: errors.New("kv unavailable")}
	svc.overrides = fake

	result, err := svc.CleanupAccount(t.Context(), testAccount)
	assert.Error(t, err)
	require.NotNil(t, result)
	assert.Equal(t, OverrideCleanupFailed, result.Disposition)
}
