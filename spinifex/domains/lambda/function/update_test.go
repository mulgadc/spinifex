package function

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const updateDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// mustServe creates "hello" with stored code and activates it.
func mustServe(t *testing.T, svc *MetaService) *GetResponse {
	t.Helper()
	rev := mustCreate(t, svc, "hello", "f1")
	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	require.NoError(t, err)
	svc.SetReadiness(func(context.Context, Record) error { return nil })
	require.NoError(t, svc.ActivatePending(t.Context()))
	got := mustGet(t, svc, "hello")
	require.Equal(t, StateActive, got.Record.State)
	return got
}

func beginReq(revision uint64, codeRevisionID string) *CodeUpdateBeginRequest {
	return &CodeUpdateBeginRequest{Region: testRegion, FunctionName: "hello", FunctionID: "f1", Revision: revision, RevisionID: codeRevisionID}
}

func commitReq(revision uint64, codeRevisionID string) *CodeUpdatedRequest {
	return &CodeUpdatedRequest{Region: testRegion, FunctionName: "hello", FunctionID: "f1", Revision: revision,
		RevisionID: codeRevisionID, SHA256: updateDigest, Size: 12}
}

func uploadOf(t *testing.T, svc *MetaService, codeRevisionID string) (Upload, bool) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), KVUploadKey(testRegion, "f1", codeRevisionID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Upload{}, false
	}
	require.NoError(t, err)
	var up Upload
	require.NoError(t, json.Unmarshal(entry.Value(), &up))
	return up, true
}

func TestCodeUpdate_CommitChangesOnlyTheCodeReference(t *testing.T) {
	svc, c := newTestService(t)
	before := mustServe(t, svc)
	c.t = c.t.Add(time.Minute)

	_, err := svc.BeginCodeUpdate(t.Context(), beginReq(before.Revision, "rev-2"), testAccount)
	require.NoError(t, err)
	assert.Equal(t, before.Revision, mustGet(t, svc, "hello").Revision, "beginning an update leaves the record as it was")
	up, ok := uploadOf(t, svc, "rev-2")
	require.True(t, ok)
	assert.Equal(t, UploadOwed, up.State)

	resp, err := svc.CodeUpdated(t.Context(), commitReq(before.Revision, "rev-2"), testAccount)
	require.NoError(t, err)
	got := mustGet(t, svc, "hello")
	assert.Equal(t, resp.Revision, got.Revision)
	assert.Equal(t, Code{RevisionID: "rev-2", SHA256: updateDigest, Size: 12, State: CodeStored}, got.Record.Code)
	assert.Equal(t, c.t, got.Record.LastModified)

	want := before.Record
	want.Code, want.LastModified = got.Record.Code, got.Record.LastModified
	assert.Equal(t, want, got.Record, "identity, configuration and state are unchanged")

	up, _ = uploadOf(t, svc, "rev-2")
	assert.Equal(t, UploadAttached, up.State)
	assert.Equal(t, resp.Revision, up.AttachedRevision)
	old, ok := uploadOf(t, svc, "rev-f1")
	require.True(t, ok, "the previous copy stays tracked")
	assert.Equal(t, UploadAttached, old.State)
}

func TestCodeUpdate_OnlyOneOfTwoUpdatesFromTheSameRevisionCommits(t *testing.T) {
	svc, _ := newTestService(t)
	before := mustServe(t, svc)
	for _, id := range []string{"rev-a", "rev-b"} {
		_, err := svc.BeginCodeUpdate(t.Context(), beginReq(before.Revision, id), testAccount)
		require.NoError(t, err)
	}
	_, err := svc.CodeUpdated(t.Context(), commitReq(before.Revision, "rev-b"), testAccount)
	require.NoError(t, err)
	_, err = svc.CodeUpdated(t.Context(), commitReq(before.Revision, "rev-a"), testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)

	assert.Equal(t, "rev-b", mustGet(t, svc, "hello").Record.Code.RevisionID)
	loser, _ := uploadOf(t, svc, "rev-a")
	assert.Equal(t, UploadOwed, loser.State, "the losing copy stays owed for cleanup")

	_, err = svc.BeginCodeUpdate(t.Context(), beginReq(before.Revision, "rev-c"), testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	_, ok := uploadOf(t, svc, "rev-c")
	assert.False(t, ok, "an update from a stale revision records nothing")
}

// Both commits read the same record before either writes: the loser is held
// between its checks and its conditional write while the winner commits.
func TestCodeUpdate_InterleavedCommitLosesTheConditionalWrite(t *testing.T) {
	svc, c := newTestService(t)
	before := mustServe(t, svc)
	for _, id := range []string{"rev-a", "rev-b"} {
		_, err := svc.BeginCodeUpdate(t.Context(), beginReq(before.Revision, id), testAccount)
		require.NoError(t, err)
	}
	paused, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	svc.now = func() time.Time {
		if calls.Add(1) == 1 {
			close(paused)
			<-release
		}
		return c.now()
	}
	loserErr := make(chan error, 1)
	go func() {
		_, err := svc.CodeUpdated(t.Context(), commitReq(before.Revision, "rev-a"), testAccount)
		loserErr <- err
	}()
	<-paused
	won, err := svc.CodeUpdated(t.Context(), commitReq(before.Revision, "rev-b"), testAccount)
	require.NoError(t, err)
	close(release)
	testutil.RequireAWSCode(t, <-loserErr, awserrors.ErrorResourceConflictException)

	got := mustGet(t, svc, "hello")
	assert.Equal(t, won.Revision, got.Revision)
	assert.Equal(t, "rev-b", got.Record.Code.RevisionID)
	winner, _ := uploadOf(t, svc, "rev-b")
	assert.Equal(t, UploadAttached, winner.State)
	assert.Equal(t, won.Revision, winner.AttachedRevision)
	loser, ok := uploadOf(t, svc, "rev-a")
	require.True(t, ok, "the losing copy stays tracked")
	assert.Equal(t, UploadOwed, loser.State)
}

func TestCodeUpdate_CommitNeedsItsOwnOwedUpload(t *testing.T) {
	svc, _ := newTestService(t)
	before := mustServe(t, svc)
	_, err := svc.CodeUpdated(t.Context(), commitReq(before.Revision, "rev-never-begun"), testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)

	_, err = svc.BeginCodeUpdate(t.Context(), beginReq(before.Revision, "rev-f1"), testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)

	other := commitReq(before.Revision, "rev-2")
	_, err = svc.BeginCodeUpdate(t.Context(), beginReq(before.Revision, "rev-2"), testAccount)
	require.NoError(t, err)
	other.FunctionID = "f-other"
	_, err = svc.CodeUpdated(t.Context(), other, testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, before.Revision, mustGet(t, svc, "hello").Revision)
}

func TestCodeUpdate_RefusesAFunctionThatIsNotServing(t *testing.T) {
	svc, _ := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	_, err := svc.BeginCodeUpdate(t.Context(), beginReq(rev, "rev-2"), testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	_, ok := uploadOf(t, svc, "rev-2")
	assert.False(t, ok)

	ghost := beginReq(1, "rev-2")
	ghost.FunctionName = "ghost"
	_, err = svc.BeginCodeUpdate(t.Context(), ghost, testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestCodeUpdate_RefusesMalformedRequests(t *testing.T) {
	svc, _ := newTestService(t)
	before := mustServe(t, svc)
	begins := map[string]func(*CodeUpdateBeginRequest){
		"region":      func(r *CodeUpdateBeginRequest) { r.Region = "Bad Region" },
		"name":        func(r *CodeUpdateBeginRequest) { r.FunctionName = "a/b" },
		"function id": func(r *CodeUpdateBeginRequest) { r.FunctionID = "" },
		"revision id": func(r *CodeUpdateBeginRequest) { r.RevisionID = "../x" },
	}
	for name, mutate := range begins {
		req := beginReq(before.Revision, "rev-2")
		mutate(req)
		t.Run("begin "+name, func(t *testing.T) {
			_, err := svc.BeginCodeUpdate(t.Context(), req, testAccount)
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		})
	}
	commits := map[string]func(*CodeUpdatedRequest){
		"digest": func(r *CodeUpdatedRequest) { r.SHA256 = "xyz" },
		"size":   func(r *CodeUpdatedRequest) { r.Size = 0 },
	}
	for name, mutate := range commits {
		req := commitReq(before.Revision, "rev-2")
		mutate(req)
		t.Run("commit "+name, func(t *testing.T) {
			_, err := svc.CodeUpdated(t.Context(), req, testAccount)
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		})
	}
	assert.Equal(t, before.Revision, mustGet(t, svc, "hello").Revision)
}
