package function

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upload reads the tracking record for functionID, or reports it absent.
func upload(t *testing.T, svc *MetaService, functionID string) (Upload, bool) {
	t.Helper()
	return uploadIn(t, svc, testRegion, functionID)
}

func uploadIn(t *testing.T, svc *MetaService, region, functionID string) (Upload, bool) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), KVUploadKey(region, functionID, "rev-"+functionID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Upload{}, false
	}
	require.NoError(t, err)
	var up Upload
	require.NoError(t, json.Unmarshal(entry.Value(), &up))
	return up, true
}

func requireOwed(t *testing.T, svc *MetaService, functionID string) {
	t.Helper()
	up, ok := upload(t, svc, functionID)
	require.True(t, ok, "upload record for %s is missing", functionID)
	assert.Equal(t, UploadOwed, up.State)
}

func TestUpload_CreateRecordsItOwedAndCodeStoredAttachesIt(t *testing.T) {
	svc, c := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	up, ok := upload(t, svc, "f1")
	require.True(t, ok)
	assert.Equal(t, Upload{FormatVersion: FormatVersion, AccountID: testAccount, Region: testRegion, FunctionName: "hello",
		FunctionID: "f1", RevisionID: "rev-f1", State: UploadOwed, CreatedAt: c.t}, up)

	stored, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	require.NoError(t, err)
	up, ok = upload(t, svc, "f1")
	require.True(t, ok)
	assert.Equal(t, UploadAttached, up.State)
	assert.Equal(t, stored.Revision, up.AttachedRevision)
}

func TestUpload_IsRecordedBeforeTheNameIsClaimed(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), KVUploadKey(testRegion, "f1", "rev-f1"), []byte(`{}`))
	require.NoError(t, err)

	_, err = svc.Create(t.Context(), createReq("hello", "f1"), testAccount)
	require.Error(t, err)
	assert.False(t, mustGet(t, svc, "hello").Found, "no claim exists when the upload could not be recorded")
}

func TestUpload_RefusedDuplicateLeavesNoRecord(t *testing.T) {
	svc, _ := newTestService(t)
	mustCreate(t, svc, "hello", "f1")
	_, err := svc.Create(t.Context(), createReq("hello", "f2"), testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	_, ok := upload(t, svc, "f2")
	assert.False(t, ok, "a create refused before any claim can never be written to")
	requireOwed(t, svc, "f1")
}

func TestUpload_RollbackKeepsItOwed(t *testing.T) {
	svc, _ := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	_, err := svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: rev}, testAccount)
	require.NoError(t, err)
	assert.False(t, mustGet(t, svc, "hello").Found)
	requireOwed(t, svc, "f1")
}

func TestUpload_ExpiredClaimIsNotTakenAsCompletion(t *testing.T) {
	svc, c := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	c.t = c.t.Add(ClaimTTL + time.Second)
	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	requireOwed(t, svc, "f1")
}

// The original writer stalls past its claim, a second create takes the name,
// and the original then finishes its write and reports it.
func TestUpload_TakeoverAndLateWriterLeaveTheOriginalOwed(t *testing.T) {
	svc, c := newTestService(t)
	oldRev := mustCreate(t, svc, "hello", "f1")
	c.t = c.t.Add(ClaimTTL + time.Second)
	newRev := mustCreate(t, svc, "hello", "f2")
	requireOwed(t, svc, "f1")
	requireOwed(t, svc, "f2")

	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: oldRev, SHA256: testDigest, Size: 9}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: oldRev}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	requireOwed(t, svc, "f1")

	_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: newRev, SHA256: testDigest, Size: 9}, testAccount)
	require.NoError(t, err)
	up, _ := upload(t, svc, "f2")
	assert.Equal(t, UploadAttached, up.State)
	requireOwed(t, svc, "f1")
}

func TestUpload_RefusesIdentifiersThatCannotBeKeys(t *testing.T) {
	svc, _ := newTestService(t)
	for _, id := range []string{"a.b", "a b", "a*"} {
		_, err := svc.Create(t.Context(), createReq("hello", id), testAccount)
		requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
	assert.False(t, mustGet(t, svc, "hello").Found)
}

func TestUpload_SameIdentifiersInTwoRegionsAreTrackedApart(t *testing.T) {
	svc, _ := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	other := createReq("hello", "f1")
	other.Region = "us-east-1"
	_, err := svc.Create(t.Context(), other, testAccount)
	require.NoError(t, err)

	_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	require.NoError(t, err)
	here, ok := uploadIn(t, svc, testRegion, "f1")
	require.True(t, ok)
	assert.Equal(t, UploadAttached, here.State)
	there, ok := uploadIn(t, svc, "us-east-1", "f1")
	require.True(t, ok)
	assert.Equal(t, UploadOwed, there.State)
	assert.Equal(t, "us-east-1", there.Region)
}

// An upload record the owner cannot vouch for refuses CodeStored, and neither
// it nor the function record changes.
func TestUpload_CodeStoredRefusesAnUnrecognisedRecord(t *testing.T) {
	asJSON := func(mutate func(*Upload)) func([]byte) []byte {
		return func(b []byte) []byte {
			var up Upload
			if err := json.Unmarshal(b, &up); err != nil {
				panic(err)
			}
			mutate(&up)
			out, err := json.Marshal(up)
			if err != nil {
				panic(err)
			}
			return out
		}
	}
	for name, rewrite := range map[string]func([]byte) []byte{
		"missing":          nil,
		"unreadable":       func([]byte) []byte { return []byte("not json") },
		"future format":    asJSON(func(u *Upload) { u.FormatVersion = FormatVersion + 1 }),
		"other function":   asJSON(func(u *Upload) { u.FunctionName = "other" }),
		"other account":    asJSON(func(u *Upload) { u.AccountID = "000000000002" }),
		"already attached": asJSON(func(u *Upload) { u.State, u.AttachedRevision = UploadAttached, 7 }),
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := newTestService(t)
			rev := mustCreate(t, svc, "hello", "f1")
			kv, err := svc.bucket(t.Context(), testAccount)
			require.NoError(t, err)
			key := KVUploadKey(testRegion, "f1", "rev-f1")
			entry, err := kv.Get(t.Context(), key)
			require.NoError(t, err)
			if rewrite == nil {
				require.NoError(t, kv.Delete(t.Context(), key))
			} else {
				_, err = kv.Update(t.Context(), key, rewrite(entry.Value()), entry.Revision())
				require.NoError(t, err)
			}
			before, err := kv.Get(t.Context(), key)
			if rewrite == nil {
				require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
			} else {
				require.NoError(t, err)
			}

			_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
			requireCode(t, err, awserrors.ErrorServiceException)

			fn := mustGet(t, svc, "hello")
			assert.Equal(t, rev, fn.Revision, "the function record is unchanged")
			assert.Equal(t, CodeStoring, fn.Record.Code.State)
			after, err := kv.Get(t.Context(), key)
			if rewrite == nil {
				require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "no record is invented")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, before.Revision(), after.Revision())
			assert.Equal(t, before.Value(), after.Value())
		})
	}
}
