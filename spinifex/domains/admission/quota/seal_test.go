package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTerminationChecker answers AccountTerminating from a fixed set, for
// tests that exercise the KV paths rather than IAM itself.
type stubTerminationChecker struct {
	terminating map[string]bool
	err         error
}

func (c *stubTerminationChecker) AccountTerminating(_ context.Context, accountID string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.terminating[accountID], nil
}

// terminatingChecker answers true for exactly accountID.
func terminatingChecker(accountID string) *stubTerminationChecker {
	return &stubTerminationChecker{terminating: map[string]bool{accountID: true}}
}

// newSealService builds a Service over a real JetStream KV bucket with the
// override bucket already attached, mirroring a quota-enabled gateway.
func newSealService(t *testing.T) (*Service, jetstream.JetStream) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  KVBucketAccountQuota,
		History: 1,
	})
	require.NoError(t, err)
	usage, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  KVBucketAccountUsage,
		History: 1,
	})
	require.NoError(t, err)

	svc := New(baseLimits(), usage)
	svc.SetOverrides(kv)
	svc.SetJetStream(js)
	return svc, js
}

// newDisabledSealService builds a Service with only the JetStream client
// wired, mirroring a quota-disabled gateway: the override bucket does not
// exist until a seal creates it.
func newDisabledSealService(t *testing.T) (*Service, jetstream.JetStream) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	svc := New(baseLimits(), nil)
	svc.SetJetStream(js)
	return svc, js
}

// lastOverrideSeq reports the override stream's last sequence, so a test can
// assert a failed attempt wrote nothing.
func lastOverrideSeq(t *testing.T, js jetstream.JetStream) uint64 {
	t.Helper()
	stream, err := js.Stream(context.Background(), "KV_"+KVBucketAccountQuota)
	require.NoError(t, err)
	info, err := stream.Info(context.Background())
	require.NoError(t, err)
	return info.State.LastSeq
}

// mutationCase is one of PutAccountQuota's two mutations, called uniformly so
// a test can table over them.
type mutationCase struct {
	name string
	call func(svc *Service, ctx context.Context) error
}

func mutationTable() []mutationCase {
	return []mutationCase{
		{"Set", func(svc *Service, ctx context.Context) error {
			return svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(32)}, "operator")
		}},
		{"Clear", func(svc *Service, ctx context.Context) error {
			return svc.PutAccountQuota(ctx, testAccount, Overrides{}, "operator")
		}},
	}
}

// seedOverrideState prepares testAccount's record for a race test: present
// (an active record), absent (never written) or tombstone (written then
// deleted, leaving a delete marker Get hides).
func seedOverrideState(t *testing.T, svc *Service, state string) {
	t.Helper()
	ctx := context.Background()
	switch state {
	case "present":
		require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))
	case "tombstone":
		require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))
		require.NoError(t, svc.overrides.Delete(ctx, testAccount))
	case "absent":
	default:
		t.Fatalf("unknown seed state %q", state)
	}
}

// sealRaceKV wraps a real KeyValue so a test can inject a deterministic race
// between a caller's read and its guarded write (Create, Update or Delete),
// instead of relying on goroutine timing.
type sealRaceKV struct {
	jetstream.KeyValue

	writeCalls  int
	beforeWrite func(attempt int) error
}

func (f *sealRaceKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	f.writeCalls++
	if f.beforeWrite != nil {
		if err := f.beforeWrite(f.writeCalls); err != nil {
			return 0, err
		}
	}
	return f.KeyValue.Create(ctx, key, value, opts...)
}

func (f *sealRaceKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	f.writeCalls++
	if f.beforeWrite != nil {
		if err := f.beforeWrite(f.writeCalls); err != nil {
			return 0, err
		}
	}
	return f.KeyValue.Update(ctx, key, value, revision)
}

func (f *sealRaceKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	f.writeCalls++
	if f.beforeWrite != nil {
		if err := f.beforeWrite(f.writeCalls); err != nil {
			return err
		}
	}
	return f.KeyValue.Delete(ctx, key, opts...)
}

// staleGetKV always answers Get with a fixed snapshot taken at construction,
// so a caller's CAS keeps retrying against a revision that can never win.
type staleGetKV struct {
	jetstream.KeyValue

	entry jetstream.KeyValueEntry
}

func (f *staleGetKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return f.entry, nil
}

// staleNotFoundKV always answers Get as absent, regardless of what the real
// bucket holds.
type staleNotFoundKV struct {
	jetstream.KeyValue
}

func (f *staleNotFoundKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}

// errGetKV always fails Get with a fixed store error.
type errGetKV struct {
	jetstream.KeyValue

	err error
}

func (f *errGetKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, f.err
}

func decodeOverride(t *testing.T, entry jetstream.KeyValueEntry) overrideRecord {
	t.Helper()
	var rec overrideRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	return rec
}

// A seal landing between a mutation's read and its guarded write is not
// outlived: the mutation's retry sees the seal and is rejected, and nothing
// it wrote survives. Clear on an absent or tombstoned key never reaches a
// write at all (see TestClearOnMissingKeyWritesNothing), so only the four
// cells with a real write are exercised here.
func TestSealRaceMutation_SealLandsBetweenReadAndWrite(t *testing.T) {
	cases := []struct {
		name  string
		state string
		call  func(svc *Service, ctx context.Context) error
	}{
		{"Set_present", "present", mutationTable()[0].call},
		{"Set_absent", "absent", mutationTable()[0].call},
		{"Set_tombstone", "tombstone", mutationTable()[0].call},
		{"Clear_present", "present", mutationTable()[1].call},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSealService(t)
			ctx := context.Background()
			seedOverrideState(t, svc, tc.state)

			realKV := svc.overrides
			sealer := New(baseLimits(), nil)
			sealer.SetOverrides(realKV)
			sealer.SetAccountTerminationChecker(terminatingChecker(testAccount))

			var sealed *OverrideSeal
			race := &sealRaceKV{KeyValue: realKV}
			race.beforeWrite = func(attempt int) error {
				if attempt != 1 {
					return nil
				}
				var sealErr error
				sealed, sealErr = sealer.SealAccountOverrides(ctx, testAccount)
				return sealErr
			}
			svc.overrides = race

			err := tc.call(svc, ctx)
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked), "got %v", err)
			require.NotNil(t, sealed)

			entry, err := realKV.Get(ctx, testAccount)
			require.NoError(t, err)
			assert.Equal(t, OverrideStateSealed, decodeOverride(t, entry).State)
			assert.Equal(t, sealed.Revision, entry.Revision(), "no write landed after the seal")
		})
	}
}

// A mutation landing inside a seal's read-write window does not survive it:
// the seal retries against the mutation's write and wins, leaving the sealed
// value with no overridden dimension.
func TestSealRaceMutation_MutationLandsBetweenSealReadAndWrite(t *testing.T) {
	for _, m := range mutationTable() {
		for _, state := range []string{"present", "absent", "tombstone"} {
			t.Run(m.name+"_"+state, func(t *testing.T) {
				svc, _ := newSealService(t)
				ctx := context.Background()
				seedOverrideState(t, svc, state)

				realKV := svc.overrides
				race := &sealRaceKV{KeyValue: realKV}
				race.beforeWrite = func(attempt int) error {
					if attempt != 1 {
						return nil
					}
					_ = m.call(svc, ctx)
					return nil
				}

				sealer := New(baseLimits(), nil)
				sealer.SetOverrides(race)
				sealer.SetAccountTerminationChecker(terminatingChecker(testAccount))

				seal, err := sealer.SealAccountOverrides(ctx, testAccount)
				require.NoError(t, err)

				entry, err := realKV.Get(ctx, testAccount)
				require.NoError(t, err)
				rec := decodeOverride(t, entry)
				assert.Equal(t, OverrideStateSealed, rec.State)
				assert.Nil(t, rec.VCPUs)
				assert.Equal(t, seal.Revision, entry.Revision())
			})
		}
	}
}

// Neither mutation can act on an already-sealed record.
func TestMutationOnSealedRecordIsRejected(t *testing.T) {
	for _, m := range mutationTable() {
		t.Run(m.name, func(t *testing.T) {
			svc, _ := newSealService(t)
			ctx := context.Background()
			svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
			_, err := svc.SealAccountOverrides(ctx, testAccount)
			require.NoError(t, err)

			before, err := svc.overrides.Get(ctx, testAccount)
			require.NoError(t, err)

			err = m.call(svc, ctx)
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked), "got %v", err)

			after, err := svc.overrides.Get(ctx, testAccount)
			require.NoError(t, err)
			assert.Equal(t, before.Revision(), after.Revision())
		})
	}
}

// Every override mutation races one seal on a real bucket; afterwards the
// record is sealed and stays sealed, so no success among them could have
// landed after the seal.
func TestSealConcurrentMutations(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))

	mutations := mutationTable()
	errs := make([]error, len(mutations))
	var wg sync.WaitGroup
	for i, m := range mutations {
		wg.Add(1)
		go func(i int, m mutationCase) {
			defer wg.Done()
			errs[i] = m.call(svc, ctx)
		}(i, m)
	}

	var sealResult *OverrideSeal
	var sealErr error
	wg.Go(func() {
		sealResult, sealErr = svc.SealAccountOverrides(ctx, testAccount)
	})
	wg.Wait()

	require.NoError(t, sealErr)
	require.NotNil(t, sealResult)
	for i, err := range errs {
		if err != nil {
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked), "mutation %s: got %v", mutations[i].name, err)
		}
	}

	entry, err := svc.overrides.Get(ctx, testAccount)
	require.NoError(t, err)
	assert.Equal(t, OverrideStateSealed, decodeOverride(t, entry).State)
	assert.Equal(t, sealResult.Revision, entry.Revision())

	_, _, err = svc.GetAccountQuota(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
}

// A Get pinned to a revision the record has already moved past exhausts the
// mutation's retries without ever writing.
func TestMutationStaleReadExhaustsWithoutWriting(t *testing.T) {
	svc, js := newSealService(t)
	ctx := context.Background()

	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))
	stale, err := svc.overrides.Get(ctx, testAccount)
	require.NoError(t, err)

	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(16)}, "operator"))

	before := lastOverrideSeq(t, js)
	svc.overrides = &staleGetKV{KeyValue: svc.overrides, entry: stale}

	err = svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(32)}, "operator")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastOverrideSeq(t, js))
}

// A Get that always reports absent, while a sealed record actually exists,
// never gets to recreate it: every Create attempt conflicts.
func TestMutationStaleAbsentDoesNotRecreate(t *testing.T) {
	svc, js := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
	_, err := svc.SealAccountOverrides(ctx, testAccount)
	require.NoError(t, err)

	before := lastOverrideSeq(t, js)
	svc.overrides = &staleNotFoundKV{KeyValue: svc.overrides}

	err = svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastOverrideSeq(t, js))
}

// A revision that keeps moving under sustained concurrent writes must not let
// the seal spin forever: it gives up, bounded by its attempt budget, and its
// own write never lands.
func TestSealSustainedConflictFailsWithoutWriting(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))

	realKV := svc.overrides
	race := &sealRaceKV{KeyValue: realKV}
	race.beforeWrite = func(int) error {
		data, merr := json.Marshal(&overrideRecord{Overrides: Overrides{VCPUs: ptr(16)}})
		require.NoError(t, merr)
		_, perr := realKV.Put(ctx, testAccount, data)
		return perr
	}
	svc.overrides = race

	_, err := svc.SealAccountOverrides(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, sealMaxAttempts, race.writeCalls)

	entry, err := realKV.Get(ctx, testAccount)
	require.NoError(t, err)
	assert.NotEqual(t, OverrideStateSealed, decodeOverride(t, entry).State, "the seal's own write never landed")
}

// A store error on the seal's read reports failed, never written.
func TestSealStoreErrorWritesNothing(t *testing.T) {
	svc, js := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))

	before := lastOverrideSeq(t, js)
	svc.overrides = &errGetKV{KeyValue: svc.overrides, err: errors.New("kv unavailable")}

	_, err := svc.SealAccountOverrides(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastOverrideSeq(t, js))
}

func TestIsRevisionConflictRecognisesTombstoneRetry(t *testing.T) {
	assert.True(t, isRevisionConflict(jetstream.ErrKeyExists))
	assert.True(t, isRevisionConflict(jetstream.ErrKeyRevisionMismatch))
	assert.True(t, isRevisionConflict(&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamWrongLastSequence}))
	assert.True(t, isRevisionConflict(&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamWrongLastSequenceConstant}))
	assert.True(t, isRevisionConflict(fmt.Errorf("wrapped: %w", &jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamWrongLastSequence})))
	assert.False(t, isRevisionConflict(errors.New("unrelated failure")))
	assert.False(t, isRevisionConflict(nil))
}

// Clearing never writes when there is nothing active to clear: an absent key,
// a tombstoned key, and a sealed key hidden behind a stale not-found Get all
// leave the stream exactly where they found it.
func TestClearOnMissingKeyWritesNothing(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		svc, js := newSealService(t)
		ctx := context.Background()
		before := lastOverrideSeq(t, js)

		require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{}, "operator"))
		assert.Equal(t, before, lastOverrideSeq(t, js))
	})

	t.Run("tombstone", func(t *testing.T) {
		svc, js := newSealService(t)
		ctx := context.Background()
		require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))
		require.NoError(t, svc.overrides.Delete(ctx, testAccount))
		before := lastOverrideSeq(t, js)

		require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{}, "operator"))
		assert.Equal(t, before, lastOverrideSeq(t, js))
	})

	t.Run("sealed_hidden_by_stale_not_found", func(t *testing.T) {
		svc, js := newSealService(t)
		ctx := context.Background()
		svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
		sealed, err := svc.SealAccountOverrides(ctx, testAccount)
		require.NoError(t, err)
		before := lastOverrideSeq(t, js)

		realKV := svc.overrides
		svc.overrides = &staleNotFoundKV{KeyValue: realKV}

		require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{}, "operator"))
		assert.Equal(t, before, lastOverrideSeq(t, js))

		entry, err := realKV.Get(ctx, testAccount)
		require.NoError(t, err)
		rec := decodeOverride(t, entry)
		assert.Equal(t, OverrideStateSealed, rec.State)
		assert.Equal(t, sealed.Revision, entry.Revision())
	})
}

// A seal against an absent or a tombstoned key creates the sealed record.
func TestSealOnMissingKeyCreatesSealedRecord(t *testing.T) {
	for _, state := range []string{"absent", "tombstone"} {
		t.Run(state, func(t *testing.T) {
			svc, _ := newSealService(t)
			ctx := context.Background()
			svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
			if state == "tombstone" {
				require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))
				require.NoError(t, svc.overrides.Delete(ctx, testAccount))
			}
			_, err := svc.overrides.Get(ctx, testAccount)
			require.ErrorIs(t, err, jetstream.ErrKeyNotFound)

			seal, err := svc.SealAccountOverrides(ctx, testAccount)
			require.NoError(t, err)

			entry, err := svc.overrides.Get(ctx, testAccount)
			require.NoError(t, err)
			assert.Equal(t, seal.Revision, entry.Revision())
			assert.Equal(t, OverrideStateSealed, decodeOverride(t, entry).State)
		})
	}
}

// alwaysConflictThenErrorKV answers Get as absent and every Create attempt
// with a conflict, then a store error on the final attempt: a recording
// wrapper proving the seal tried a real write on every attempt before giving
// up, rather than ever reporting success from an observed absence.
type alwaysConflictThenErrorKV struct {
	jetstream.KeyValue

	calls int
}

func (f *alwaysConflictThenErrorKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}

func (f *alwaysConflictThenErrorKV) Create(context.Context, string, []byte, ...jetstream.KVCreateOpt) (uint64, error) {
	f.calls++
	if f.calls < sealMaxAttempts {
		return 0, jetstream.ErrKeyExists
	}
	return 0, errors.New("store unavailable")
}

func TestSealNeverSucceedsWithoutAWrite(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
	fake := &alwaysConflictThenErrorKV{}
	svc.overrides = fake

	seal, err := svc.SealAccountOverrides(ctx, testAccount)
	assert.Error(t, err)
	assert.Nil(t, seal)
	assert.Equal(t, sealMaxAttempts, fake.calls, "every attempt must have tried a real write before giving up")
}

// An account IAM does not hold as TERMINATING refuses the seal before any
// override-KV write, whether the key is present or absent.
func TestSealRefusedForActiveAccount(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		t.Run(state, func(t *testing.T) {
			svc, js := newSealService(t)
			ctx := context.Background()
			if state == "present" {
				require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"))
			}
			svc.SetAccountTerminationChecker(&stubTerminationChecker{terminating: map[string]bool{}})

			before := lastOverrideSeq(t, js)
			_, err := svc.SealAccountOverrides(ctx, testAccount)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorIncorrectState), "got %v", err)
			assert.Equal(t, before, lastOverrideSeq(t, js))
		})
	}
}

func TestSealRefusedForMissingIAMRecord(t *testing.T) {
	svc, js := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(&stubTerminationChecker{terminating: map[string]bool{}})

	before := lastOverrideSeq(t, js)
	_, err := svc.SealAccountOverrides(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorIncorrectState), "got %v", err)
	assert.Equal(t, before, lastOverrideSeq(t, js))
}

func TestSealRefusedWhenIAMReadFails(t *testing.T) {
	svc, js := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(&stubTerminationChecker{err: errors.New("iam unavailable")})

	before := lastOverrideSeq(t, js)
	_, err := svc.SealAccountOverrides(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastOverrideSeq(t, js))
}

func TestSealRefusedWithNoCheckerWired(t *testing.T) {
	svc, js := newSealService(t)
	ctx := context.Background()

	before := lastOverrideSeq(t, js)
	_, err := svc.SealAccountOverrides(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastOverrideSeq(t, js))
}

func TestSealAcceptedForTerminatingAccount(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))

	seal, err := svc.SealAccountOverrides(ctx, testAccount)
	require.NoError(t, err)
	assert.NotZero(t, seal.Revision)

	entry, err := svc.overrides.Get(ctx, testAccount)
	require.NoError(t, err)
	assert.Equal(t, seal.Revision, entry.Revision())
}

// A refused seal, on a gateway that disabled quotas and so holds no override
// bucket, never creates one.
func TestSealRefusalDoesNotCreateBucket(t *testing.T) {
	t.Run("not_terminating", func(t *testing.T) {
		svc, js := newDisabledSealService(t)
		ctx := context.Background()
		svc.SetAccountTerminationChecker(&stubTerminationChecker{terminating: map[string]bool{}})

		_, err := svc.SealAccountOverrides(ctx, testAccount)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorIncorrectState), "got %v", err)
		_, err = js.KeyValue(context.Background(), KVBucketAccountQuota)
		assert.ErrorIs(t, err, jetstream.ErrBucketNotFound)
	})

	t.Run("iam_read_fails", func(t *testing.T) {
		svc, js := newDisabledSealService(t)
		ctx := context.Background()
		svc.SetAccountTerminationChecker(&stubTerminationChecker{err: errors.New("iam unavailable")})

		_, err := svc.SealAccountOverrides(ctx, testAccount)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
		_, err = js.KeyValue(context.Background(), KVBucketAccountQuota)
		assert.ErrorIs(t, err, jetstream.ErrBucketNotFound)
	})
}

// A seal accepted on a gateway whose quotas were disabled at startup creates
// the override bucket at seal time, never at startup.
func TestSealOnQuotaDisabledGatewayCreatesBucket(t *testing.T) {
	svc, js := newDisabledSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))

	seal, err := svc.SealAccountOverrides(ctx, testAccount)
	require.NoError(t, err)

	kv, err := js.KeyValue(context.Background(), KVBucketAccountQuota)
	require.NoError(t, err)
	entry, err := kv.Get(context.Background(), testAccount)
	require.NoError(t, err)
	assert.Equal(t, seal.Revision, entry.Revision())
	assert.Equal(t, OverrideStateSealed, decodeOverride(t, entry).State)
}

// A sealed record stays sealed after the service is rebuilt over the same
// JetStream, every read and write refuses it, and a re-seal gets a higher
// revision.
func TestSealSurvivesServiceReconstruction(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: KVBucketAccountQuota, History: 1})
	require.NoError(t, err)

	svc := New(baseLimits(), nil)
	svc.SetOverrides(kv)
	svc.SetJetStream(js)
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
	first, err := svc.SealAccountOverrides(ctx, testAccount)
	require.NoError(t, err)

	rebuiltKV, err := js.KeyValue(ctx, KVBucketAccountQuota)
	require.NoError(t, err)
	rebuilt := New(baseLimits(), nil)
	rebuilt.SetOverrides(rebuiltKV)
	rebuilt.SetJetStream(js)

	entry, err := rebuilt.overrides.Get(ctx, testAccount)
	require.NoError(t, err)
	assert.Equal(t, OverrideStateSealed, decodeOverride(t, entry).State)

	_, _, err = rebuilt.GetAccountQuota(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
	assert.True(t, awserrors.IsErrorCode(rebuilt.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"), awserrors.ErrorBlocked))
	assert.True(t, awserrors.IsErrorCode(rebuilt.PutAccountQuota(ctx, testAccount, Overrides{}, "operator"), awserrors.ErrorBlocked))

	limits, err := rebuilt.limitsFor(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
	assert.Zero(t, limits)

	rebuilt.SetAccountTerminationChecker(terminatingChecker(testAccount))
	second, err := rebuilt.SealAccountOverrides(ctx, testAccount)
	require.NoError(t, err)
	assert.Greater(t, second.Revision, first.Revision)
}

func TestSealRefusesEmptyInvalidAndGlobalAccount(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()

	_, err := svc.SealAccountOverrides(ctx, "")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorMissingParameter), "got %v", err)

	_, err = svc.SealAccountOverrides(ctx, "not-an-account")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterValue), "got %v", err)

	_, err = svc.SealAccountOverrides(ctx, awsidentifiers.GlobalAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterValue), "got %v", err)
}

func TestSealLeavesOtherAccountsUntouched(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()
	accountB := "222222222222"
	require.NoError(t, svc.PutAccountQuota(ctx, accountB, Overrides{VCPUs: ptr(8)}, "operator"))

	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
	_, err := svc.SealAccountOverrides(ctx, testAccount)
	require.NoError(t, err)

	over, _, err := svc.GetAccountQuota(ctx, accountB)
	require.NoError(t, err)
	require.NotNil(t, over.VCPUs)
	assert.Equal(t, 8, *over.VCPUs)
}

// A record written before the state field existed has none and is treated as
// active by both reads and writes; a write on it preserves the old shape.
func TestRecordWithoutStateIsActive(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()

	data, err := json.Marshal(Overrides{VCPUs: ptr(8)})
	require.NoError(t, err)
	_, err = svc.overrides.Put(ctx, testAccount, data)
	require.NoError(t, err)

	over, limits, err := svc.GetAccountQuota(ctx, testAccount)
	require.NoError(t, err)
	require.NotNil(t, over.VCPUs)
	assert.Equal(t, 8, limits.VCPUs)

	require.NoError(t, svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(16)}, "operator"))
	entry, err := svc.overrides.Get(ctx, testAccount)
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), `"state"`)
}

// A record whose state is neither active nor sealed fails every read and
// write closed, rather than being treated as active.
func TestUnknownStateFailsClosed(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()

	_, err := svc.overrides.Put(ctx, testAccount, []byte(`{"state":"frozen"}`))
	require.NoError(t, err)

	_, _, err = svc.GetAccountQuota(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)

	limits, err := svc.limitsFor(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Zero(t, limits)

	assert.True(t, awserrors.IsErrorCode(svc.PutAccountQuota(ctx, testAccount, Overrides{VCPUs: ptr(8)}, "operator"), awserrors.ErrorInternalError))
	assert.True(t, awserrors.IsErrorCode(svc.PutAccountQuota(ctx, testAccount, Overrides{}, "operator"), awserrors.ErrorInternalError))
}

// A sealed record is Blocked for admission's own resolver, which every
// caller (admin read, ELBv2 DescribeAccountLimits, every Enforce* method)
// shares.
func TestAdmissionOnSealedRecordIsBlocked(t *testing.T) {
	svc, _ := newSealService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccount))
	_, err := svc.SealAccountOverrides(ctx, testAccount)
	require.NoError(t, err)

	limits, err := svc.limitsFor(ctx, testAccount)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked), "got %v", err)
	assert.Zero(t, limits)

	assert.Error(t, svc.CheckVCPU(ctx, testAccount, 1))
	assert.True(t, awserrors.IsErrorCode(svc.CheckVCPU(ctx, testAccount, 1), awserrors.ErrorBlocked))
}

// The existing read-failure fallback in limitsFor (a decode or store error
// falls back to the configured limits) is unchanged by the seal work: it is
// distinct from a successfully read sealed record, which refuses instead.
func TestAdmissionReadFailureFallbackUnchanged(t *testing.T) {
	svc, kv := newOverrideService(t)

	_, err := kv.Put(t.Context(), testAccount, []byte("{not json"))
	require.NoError(t, err)

	limits, err := svc.limitsFor(t.Context(), testAccount)
	require.NoError(t, err, "a decode failure must still fall back, not error, from limitsFor")
	require.Equal(t, baseLimits(), limits)
}
