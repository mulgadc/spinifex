package function

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccount = "000000000001"
	testRegion  = "ap-southeast-2"
	testDigest  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestService(t *testing.T) (*MetaService, *clock) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := NewMetaService(testutil.NewJetStream(t, nc))
	c := &clock{t: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	svc.now = c.now
	return svc, c
}

func createReq(name, functionID string) *CreateRequest {
	return &CreateRequest{Region: testRegion, Record: Record{
		FunctionID: functionID, FunctionName: name, Role: "arn:aws:iam::000000000001:role/r",
		Runtime: "provided.al2023", Handler: "bootstrap", Timeout: 3, MemorySize: 128,
		Code: Code{RevisionID: "rev-" + functionID},
	}}
}

func mustCreate(t *testing.T, svc *MetaService, name, functionID string) uint64 {
	t.Helper()
	resp, err := svc.Create(t.Context(), createReq(name, functionID), testAccount)
	require.NoError(t, err)
	return resp.Revision
}

func mustGet(t *testing.T, svc *MetaService, name string) *GetResponse {
	t.Helper()
	resp, err := svc.Get(t.Context(), &GetRequest{Region: testRegion, FunctionName: name}, testAccount)
	require.NoError(t, err)
	return resp
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	got, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "no AWS code on %v", err)
	assert.Equal(t, code, got)
}

func TestMeta_CreateOwnsStateAndRejectsDuplicate(t *testing.T) {
	svc, c := newTestService(t)
	req := createReq("hello", "f1")
	req.Record.State = StateActive
	req.Record.Code.State = CodeStored
	req.Record.AccountID = "someone-else"
	resp, err := svc.Create(t.Context(), req, testAccount)
	require.NoError(t, err)

	got := mustGet(t, svc, "hello")
	require.True(t, got.Found)
	assert.Equal(t, resp.Revision, got.Revision)
	assert.Equal(t, StatePending, got.Record.State, "the daemon owns state")
	assert.Equal(t, CodeStoring, got.Record.Code.State)
	assert.Equal(t, testAccount, got.Record.AccountID, "the account comes from the request header")
	assert.Equal(t, FormatVersion, got.Record.FormatVersion)
	assert.Equal(t, c.t.Add(ClaimTTL), got.Record.ClaimExpiresAt)

	_, err = svc.Create(t.Context(), createReq("hello", "f2"), testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, "f1", mustGet(t, svc, "hello").Record.FunctionID, "the first record is untouched")

	other, err := svc.Create(t.Context(), createReq("hello", "f3"), "000000000002")
	require.NoError(t, err, "names are per account")
	assert.NotZero(t, other.Revision)
}

func TestMeta_CreateRefusesIncompleteRecords(t *testing.T) {
	svc, _ := newTestService(t)
	for name, mutate := range map[string]func(*Record){
		"bad name":    func(r *Record) { r.FunctionName = "has space" },
		"no id":       func(r *Record) { r.FunctionID = "" },
		"no revision": func(r *Record) { r.Code.RevisionID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			req := createReq("ok", "f1")
			mutate(&req.Record)
			_, err := svc.Create(t.Context(), req, testAccount)
			requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
		})
	}
}

func TestMeta_CodeStoredUsesCAS(t *testing.T) {
	svc, _ := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")

	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev + 5, SHA256: testDigest, Size: 9}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, CodeStoring, mustGet(t, svc, "hello").Record.Code.State, "a stale revision changes nothing")

	_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: "xyz", Size: 9}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidParameterValueException)

	resp, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	require.NoError(t, err)
	got := mustGet(t, svc, "hello")
	assert.Equal(t, resp.Revision, got.Revision)
	assert.Equal(t, CodeStored, got.Record.Code.State)
	assert.Equal(t, testDigest, got.Record.Code.SHA256)
	assert.Equal(t, int64(9), got.Record.Code.Size)
	assert.Equal(t, StatePending, got.Record.State)
	assert.Equal(t, ReasonCodeCreating, got.Record.StateReasonCode)
	assert.Equal(t, ReasonCreating, got.Record.StateReason)

	_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: resp.Revision, SHA256: testDigest, Size: 9}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)

	_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "ghost", Revision: 1, SHA256: testDigest}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestMeta_RollbackDeletesOnlyStoringAtTheRightRevision(t *testing.T) {
	svc, _ := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")

	_, err := svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: rev + 1}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	require.True(t, mustGet(t, svc, "hello").Found)

	_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: rev}, testAccount)
	require.NoError(t, err)
	assert.False(t, mustGet(t, svc, "hello").Found)
	_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: rev}, testAccount)
	require.NoError(t, err, "rolling back an absent name is success")
	mustCreate(t, svc, "hello", "f2")
	assert.Equal(t, "f2", mustGet(t, svc, "hello").Record.FunctionID, "the name is free again")

	rev = mustGet(t, svc, "hello").Revision
	stored, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 1}, testAccount)
	require.NoError(t, err)
	_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: stored.Revision}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	assert.True(t, mustGet(t, svc, "hello").Found, "a stored record is never rolled back")
}

func TestMeta_GetMissingAndUnknownFormat(t *testing.T) {
	svc, _ := newTestService(t)
	assert.False(t, mustGet(t, svc, "ghost").Found)

	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	data, err := json.Marshal(Record{FormatVersion: 99, FunctionName: "future"})
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), KVFunctionKey(testRegion, "future"), data)
	require.NoError(t, err)

	_, err = svc.Get(t.Context(), &GetRequest{Region: testRegion, FunctionName: "future"}, testAccount)
	requireCode(t, err, awserrors.ErrorServiceException)
	_, err = svc.Create(t.Context(), createReq("future", "f1"), testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "future", Revision: 1}, testAccount)
	requireCode(t, err, awserrors.ErrorServiceException)
}

func TestMeta_ExpiredStoringClaimIsTakenOver(t *testing.T) {
	svc, c := newTestService(t)
	oldRev := mustCreate(t, svc, "hello", "old")

	c.t = c.t.Add(ClaimTTL - time.Second)
	_, err := svc.Create(t.Context(), createReq("hello", "new"), testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, "old", mustGet(t, svc, "hello").Record.FunctionID, "an unexpired claim holds the name")

	c.t = c.t.Add(2 * time.Second)
	newRev := mustCreate(t, svc, "hello", "new")
	got := mustGet(t, svc, "hello")
	assert.Equal(t, "new", got.Record.FunctionID)
	assert.Equal(t, "rev-new", got.Record.Code.RevisionID)
	assert.Equal(t, newRev, got.Revision)
	assert.Equal(t, c.t.Add(ClaimTTL), got.Record.ClaimExpiresAt)

	_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: oldRev, SHA256: testDigest, Size: 1}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: oldRev}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	got = mustGet(t, svc, "hello")
	assert.Equal(t, "new", got.Record.FunctionID, "the stale gateway cannot resurrect or free the old claim")
	assert.Equal(t, CodeStoring, got.Record.Code.State)
}

func TestMeta_StoredRecordIsNeverTakenOver(t *testing.T) {
	svc, c := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 1}, testAccount)
	require.NoError(t, err)

	c.t = c.t.Add(10 * ClaimTTL)
	_, err = svc.Create(t.Context(), createReq("hello", "f2"), testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, "f1", mustGet(t, svc, "hello").Record.FunctionID)
}

func TestMeta_CodeStoredAfterExpiryIsRefused(t *testing.T) {
	svc, c := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")

	c.t = c.t.Add(ClaimTTL + time.Second)
	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 1}, testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	got := mustGet(t, svc, "hello")
	assert.Equal(t, CodeStoring, got.Record.Code.State, "a refused update leaves the record unchanged")
	assert.Equal(t, rev, got.Revision)
}

func TestMeta_RegionsAreIndependent(t *testing.T) {
	svc, _ := newTestService(t)
	sydney := createReq("hello", "syd")
	oregon := createReq("hello", "pdx")
	oregon.Region = "us-west-2"
	_, err := svc.Create(t.Context(), sydney, testAccount)
	require.NoError(t, err)
	_, err = svc.Create(t.Context(), oregon, testAccount)
	require.NoError(t, err, "the same name in another Region is a different function")

	got, err := svc.Get(t.Context(), &GetRequest{Region: "us-west-2", FunctionName: "hello"}, testAccount)
	require.NoError(t, err)
	assert.Equal(t, "pdx", got.Record.FunctionID)
	assert.Equal(t, "us-west-2", got.Record.Region)
	assert.Equal(t, "syd", mustGet(t, svc, "hello").Record.FunctionID)
	assert.Equal(t, testRegion, mustGet(t, svc, "hello").Record.Region)

	_, err = svc.Create(t.Context(), createReq("hello", "again"), testAccount)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
}

func TestMeta_RefusesBadRegion(t *testing.T) {
	svc, _ := newTestService(t)
	for _, region := range []string{"", "AP-SOUTH", "a.b", "a b", "x/y"} {
		req := createReq("hello", "f1")
		req.Region = region
		_, err := svc.Create(t.Context(), req, testAccount)
		requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
		_, err = svc.Get(t.Context(), &GetRequest{Region: region, FunctionName: "hello"}, testAccount)
		requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
		_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: region, FunctionName: "hello", Revision: 1, SHA256: testDigest}, testAccount)
		requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
		_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: region, FunctionName: "hello", Revision: 1}, testAccount)
		requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
}
