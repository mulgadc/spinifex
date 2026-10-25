package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
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

// lastSettingsSeq reports the account-settings stream's last sequence, so a
// test can assert a failed attempt wrote nothing.
func lastSettingsSeq(t *testing.T, svc *AccountSettingsServiceImpl) uint64 {
	t.Helper()
	stream, err := svc.js.Stream(context.Background(), "KV_"+KVBucketAccountSettings)
	require.NoError(t, err)
	info, err := stream.Info(context.Background())
	require.NoError(t, err)
	return info.State.LastSeq
}

// mutationCase is one of the four settings mutations, called uniformly so a
// test can table over them.
type mutationCase struct {
	name string
	call func(svc *AccountSettingsServiceImpl, ctx context.Context) error
}

func mutationTable() []mutationCase {
	return []mutationCase{
		{"EnableEbs", func(svc *AccountSettingsServiceImpl, ctx context.Context) error {
			_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
			return err
		}},
		{"DisableEbs", func(svc *AccountSettingsServiceImpl, ctx context.Context) error {
			_, err := svc.DisableEbsEncryptionByDefault(ctx, &ec2.DisableEbsEncryptionByDefaultInput{}, testAccountID)
			return err
		}},
		{"EnableSerial", func(svc *AccountSettingsServiceImpl, ctx context.Context) error {
			_, err := svc.EnableSerialConsoleAccess(ctx, &ec2.EnableSerialConsoleAccessInput{}, testAccountID)
			return err
		}},
		{"DisableSerial", func(svc *AccountSettingsServiceImpl, ctx context.Context) error {
			_, err := svc.DisableSerialConsoleAccess(ctx, &ec2.DisableSerialConsoleAccessInput{}, testAccountID)
			return err
		}},
	}
}

// seedSettingsState prepares testAccountID's record for a race test: present
// (an active record), absent (never written) or tombstone (written then
// deleted, leaving a delete marker Get hides).
func seedSettingsState(t *testing.T, svc *AccountSettingsServiceImpl, state string) {
	t.Helper()
	ctx := context.Background()
	switch state {
	case "present":
		_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
		require.NoError(t, err)
	case "tombstone":
		_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
		require.NoError(t, err)
		require.NoError(t, svc.settingsKV.Delete(ctx, testAccountID))
	case "absent":
	default:
		t.Fatalf("unknown seed state %q", state)
	}
}

// sealRaceKV wraps a real KeyValue so a test can inject a deterministic race
// between a caller's read and its guarded write (Create or Update), instead
// of relying on goroutine timing.
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

func decodeSettings(t *testing.T, entry jetstream.KeyValueEntry) AccountSettingsRecord {
	t.Helper()
	var rec AccountSettingsRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	return rec
}

// A seal landing between a mutation's read and its guarded write is not
// outlived: the mutation's retry sees the seal and is rejected, and nothing
// it wrote survives.
func TestSealRaceMutation_SealLandsBetweenReadAndWrite(t *testing.T) {
	for _, m := range mutationTable() {
		for _, state := range []string{"present", "absent", "tombstone"} {
			t.Run(m.name+"_"+state, func(t *testing.T) {
				svc := setupTestAccountService(t)
				ctx := context.Background()
				seedSettingsState(t, svc, state)

				realKV := svc.settingsKV
				sealer := &AccountSettingsServiceImpl{js: svc.js, settingsKV: realKV}
				sealer.SetAccountTerminationChecker(terminatingChecker(testAccountID))

				var sealed *AccountSettingsSeal
				race := &sealRaceKV{KeyValue: realKV}
				race.beforeWrite = func(attempt int) error {
					if attempt != 1 {
						return nil
					}
					var sealErr error
					sealed, sealErr = sealer.SealAccount(ctx, testAccountID)
					return sealErr
				}
				svc.settingsKV = race

				err := m.call(svc, ctx)
				require.Error(t, err)
				assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked), "got %v", err)
				require.NotNil(t, sealed)

				entry, err := realKV.Get(ctx, testAccountID)
				require.NoError(t, err)
				assert.Equal(t, AccountSettingsStateSealed, decodeSettings(t, entry).State)
				assert.Equal(t, sealed.Revision, entry.Revision(), "no write landed after the seal")
			})
		}
	}
}

// A mutation landing inside a seal's read-write window does not survive it:
// the seal retries against the mutation's write and wins, leaving the
// sealed value with both booleans false.
func TestSealRaceMutation_MutationLandsBetweenSealReadAndWrite(t *testing.T) {
	for _, m := range mutationTable() {
		for _, state := range []string{"present", "absent", "tombstone"} {
			t.Run(m.name+"_"+state, func(t *testing.T) {
				svc := setupTestAccountService(t)
				ctx := context.Background()
				seedSettingsState(t, svc, state)

				realKV := svc.settingsKV
				race := &sealRaceKV{KeyValue: realKV}
				race.beforeWrite = func(attempt int) error {
					if attempt != 1 {
						return nil
					}
					_ = m.call(svc, ctx)
					return nil
				}

				sealer := &AccountSettingsServiceImpl{js: svc.js, settingsKV: race}
				sealer.SetAccountTerminationChecker(terminatingChecker(testAccountID))

				seal, err := sealer.SealAccount(ctx, testAccountID)
				require.NoError(t, err)

				entry, err := realKV.Get(ctx, testAccountID)
				require.NoError(t, err)
				rec := decodeSettings(t, entry)
				assert.Equal(t, AccountSettingsStateSealed, rec.State)
				assert.False(t, rec.EbsEncryptionByDefault)
				assert.False(t, rec.SerialConsoleAccess)
				assert.Equal(t, seal.Revision, entry.Revision())
			})
		}
	}
}

// None of the four mutations can act on an already-sealed record.
func TestMutationOnSealedRecordIsRejected(t *testing.T) {
	for _, m := range mutationTable() {
		t.Run(m.name, func(t *testing.T) {
			svc := setupTestAccountService(t)
			ctx := context.Background()
			svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))
			_, err := svc.SealAccount(ctx, testAccountID)
			require.NoError(t, err)

			before, err := svc.settingsKV.Get(ctx, testAccountID)
			require.NoError(t, err)

			err = m.call(svc, ctx)
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked), "got %v", err)

			after, err := svc.settingsKV.Get(ctx, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, before.Revision(), after.Revision())
		})
	}
}

// Every settings mutation races one seal on a real bucket; afterwards the
// record is sealed and stays sealed, so no success among them could have
// landed after the seal.
func TestSealConcurrentMutations(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

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

	var sealResult *AccountSettingsSeal
	var sealErr error
	wg.Go(func() {
		sealResult, sealErr = svc.SealAccount(ctx, testAccountID)
	})
	wg.Wait()

	require.NoError(t, sealErr)
	require.NotNil(t, sealResult)
	for i, err := range errs {
		if err != nil {
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked), "mutation %s: got %v", mutations[i].name, err)
		}
	}

	entry, err := svc.settingsKV.Get(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsStateSealed, decodeSettings(t, entry).State)
	assert.Equal(t, sealResult.Revision, entry.Revision())

	_, err = svc.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
}

// A Get pinned to a revision the record has already moved past exhausts the
// mutation's retries without ever writing.
func TestMutationStaleReadExhaustsWithoutWriting(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()

	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	stale, err := svc.settingsKV.Get(ctx, testAccountID)
	require.NoError(t, err)

	_, err = svc.DisableEbsEncryptionByDefault(ctx, &ec2.DisableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

	before := lastSettingsSeq(t, svc)
	svc.settingsKV = &staleGetKV{KeyValue: svc.settingsKV, entry: stale}

	_, err = svc.EnableSerialConsoleAccess(ctx, &ec2.EnableSerialConsoleAccessInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastSettingsSeq(t, svc))
}

// A Get that always reports absent, while a sealed record actually exists,
// never gets to recreate it: every Create attempt conflicts.
func TestMutationStaleAbsentDoesNotRecreate(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))
	_, err := svc.SealAccount(ctx, testAccountID)
	require.NoError(t, err)

	before := lastSettingsSeq(t, svc)
	svc.settingsKV = &staleNotFoundKV{KeyValue: svc.settingsKV}

	_, err = svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastSettingsSeq(t, svc))
}

// A revision that keeps moving under sustained concurrent writes must not
// let the seal spin forever: it gives up, bounded by its attempt budget,
// and its own write never lands.
func TestSealSustainedConflictFailsWithoutWriting(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

	realKV := svc.settingsKV
	race := &sealRaceKV{KeyValue: realKV}
	race.beforeWrite = func(int) error {
		data, merr := json.Marshal(&AccountSettingsRecord{SerialConsoleAccess: true})
		require.NoError(t, merr)
		_, perr := realKV.Put(ctx, testAccountID, data)
		return perr
	}
	svc.settingsKV = race

	_, err = svc.SealAccount(ctx, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, sealMaxAttempts, race.writeCalls)

	entry, err := realKV.Get(ctx, testAccountID)
	require.NoError(t, err)
	assert.NotEqual(t, AccountSettingsStateSealed, decodeSettings(t, entry).State, "the seal's own write never landed")
}

// A store error on the seal's read reports failed, never written.
func TestSealStoreErrorWritesNothing(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))

	before := lastSettingsSeq(t, svc)
	svc.settingsKV = &errGetKV{KeyValue: svc.settingsKV, err: errors.New("kv unavailable")}

	_, err := svc.SealAccount(ctx, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastSettingsSeq(t, svc))
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

// An account IAM does not hold as TERMINATING refuses the seal before any
// settings-KV write, whether the key is present or absent.
func TestSealRefusedForActiveAccount(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		t.Run(state, func(t *testing.T) {
			svc := setupTestAccountService(t)
			ctx := context.Background()
			if state == "present" {
				_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
				require.NoError(t, err)
			}
			svc.SetAccountTerminationChecker(&stubTerminationChecker{terminating: map[string]bool{}})

			before := lastSettingsSeq(t, svc)
			_, err := svc.SealAccount(ctx, testAccountID)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorIncorrectState), "got %v", err)
			assert.Equal(t, before, lastSettingsSeq(t, svc))
		})
	}
}

func TestSealRefusedForMissingIAMRecord(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(&stubTerminationChecker{terminating: map[string]bool{}})

	before := lastSettingsSeq(t, svc)
	_, err := svc.SealAccount(ctx, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorIncorrectState), "got %v", err)
	assert.Equal(t, before, lastSettingsSeq(t, svc))
}

func TestSealRefusedWhenIAMReadFails(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(&stubTerminationChecker{err: errors.New("iam unavailable")})

	before := lastSettingsSeq(t, svc)
	_, err := svc.SealAccount(ctx, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastSettingsSeq(t, svc))
}

func TestSealRefusedWithNoCheckerWired(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()

	before := lastSettingsSeq(t, svc)
	_, err := svc.SealAccount(ctx, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError), "got %v", err)
	assert.Equal(t, before, lastSettingsSeq(t, svc))
}

func TestSealAcceptedForTerminatingAccount(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))

	seal, err := svc.SealAccount(ctx, testAccountID)
	require.NoError(t, err)
	assert.NotZero(t, seal.Revision)

	entry, err := svc.settingsKV.Get(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, seal.Revision, entry.Revision())
}

// A sealed record stays sealed after the service is rebuilt over the same
// JetStream, and every one of the six settings actions still refuses it.
func TestSealSurvivesServiceReconstruction(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	ctx := context.Background()

	svc, err := NewAccountSettingsServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))
	first, err := svc.SealAccount(ctx, testAccountID)
	require.NoError(t, err)

	rebuilt, err := NewAccountSettingsServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)

	entry, err := rebuilt.settingsKV.Get(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, AccountSettingsStateSealed, decodeSettings(t, entry).State)

	_, err = rebuilt.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
	_, err = rebuilt.GetSerialConsoleAccessStatus(ctx, &ec2.GetSerialConsoleAccessStatusInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
	_, err = rebuilt.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
	_, err = rebuilt.DisableEbsEncryptionByDefault(ctx, &ec2.DisableEbsEncryptionByDefaultInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
	_, err = rebuilt.EnableSerialConsoleAccess(ctx, &ec2.EnableSerialConsoleAccessInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))
	_, err = rebuilt.DisableSerialConsoleAccess(ctx, &ec2.DisableSerialConsoleAccessInput{}, testAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorBlocked))

	rebuilt.SetAccountTerminationChecker(terminatingChecker(testAccountID))
	second, err := rebuilt.SealAccount(ctx, testAccountID)
	require.NoError(t, err)
	assert.Greater(t, second.Revision, first.Revision)
}

func TestSealAbsentKeyCreatesSealedRecord(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))

	_, err := svc.settingsKV.Get(ctx, testAccountID)
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)

	seal, err := svc.SealAccount(ctx, testAccountID)
	require.NoError(t, err)

	entry, err := svc.settingsKV.Get(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, seal.Revision, entry.Revision())
	assert.Equal(t, AccountSettingsStateSealed, decodeSettings(t, entry).State)
}

func TestSealRefusesEmptyInvalidAndGlobalAccount(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()

	_, err := svc.SealAccount(ctx, "")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorMissingParameter), "got %v", err)

	_, err = svc.SealAccount(ctx, "not-an-account")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterValue), "got %v", err)

	_, err = svc.SealAccount(ctx, awsidentifiers.GlobalAccountID)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterValue), "got %v", err)
}

func TestSealLeavesOtherAccountsUntouched(t *testing.T) {
	svc := setupTestAccountService(t)
	ctx := context.Background()
	accountB := "222222222222"
	_, err := svc.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, accountB)
	require.NoError(t, err)

	svc.SetAccountTerminationChecker(terminatingChecker(testAccountID))
	_, err = svc.SealAccount(ctx, testAccountID)
	require.NoError(t, err)

	out, err := svc.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{}, accountB)
	require.NoError(t, err)
	assert.True(t, *out.EbsEncryptionByDefault)
}
