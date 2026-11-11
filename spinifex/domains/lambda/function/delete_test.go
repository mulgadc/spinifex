package function

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveAs creates name with stored code under functionID and activates it.
func serveAs(t *testing.T, svc *MetaService, name, functionID string) *GetResponse {
	t.Helper()
	rev := mustCreate(t, svc, name, functionID)
	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: name, Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	require.NoError(t, err)
	svc.SetReadiness(func(context.Context, Record) error { return nil })
	require.NoError(t, svc.ActivatePending(t.Context()))
	got := mustGet(t, svc, name)
	require.Equal(t, StateActive, got.Record.State)
	return got
}

func deleteReq(got *GetResponse) *DeleteRequest {
	return &DeleteRequest{Region: testRegion, FunctionName: got.Record.FunctionName, FunctionID: got.Record.FunctionID, Revision: got.Revision}
}

// deletion reads the retained obligation for functionID, or reports it absent.
func deletion(t *testing.T, svc *MetaService, functionID string) (Deletion, bool) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), KVDeletedKey(testRegion, functionID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Deletion{}, false
	}
	require.NoError(t, err)
	var d Deletion
	require.NoError(t, json.Unmarshal(entry.Value(), &d))
	return d, true
}

func requireRetained(t *testing.T, svc *MetaService, name, functionID string) {
	t.Helper()
	d, ok := deletion(t, svc, functionID)
	require.True(t, ok, "deletion obligation for %s is missing", functionID)
	assert.Equal(t, Deletion{FormatVersion: FormatVersion, AccountID: testAccount, Region: testRegion,
		FunctionName: name, FunctionID: functionID, RecordedAt: d.RecordedAt}, d)
	_, ok = upload(t, svc, functionID)
	assert.True(t, ok, "upload record for %s was removed", functionID)
}

func TestDelete_UnbindsTheNameAndRetainsTheObligationAndUploads(t *testing.T) {
	svc, c := newTestService(t)
	got := serveAs(t, svc, "hello", "f1")
	_, err := svc.Delete(t.Context(), deleteReq(got), testAccount)
	require.NoError(t, err)

	assert.False(t, mustGet(t, svc, "hello").Found)
	requireRetained(t, svc, "hello", "f1")
	d, _ := deletion(t, svc, "f1")
	assert.Equal(t, c.now(), d.RecordedAt)
	up, _ := upload(t, svc, "f1")
	assert.Equal(t, UploadAttached, up.State, "the upload record is left exactly as it was")
}

func TestDelete_MissingFunctionIsNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.Delete(t.Context(), &DeleteRequest{Region: testRegion, FunctionName: "hello", FunctionID: "f1", Revision: 1}, testAccount)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, ok := deletion(t, svc, "f1")
	assert.False(t, ok)
}

func TestDelete_StaleRevisionOrAnotherFunctionIsRefusedWithoutAnObligation(t *testing.T) {
	svc, _ := newTestService(t)
	got := serveAs(t, svc, "hello", "f1")
	for name, req := range map[string]*DeleteRequest{
		"stale revision": {Region: testRegion, FunctionName: "hello", FunctionID: "f1", Revision: got.Revision - 1},
		"other function": {Region: testRegion, FunctionName: "hello", FunctionID: "f0", Revision: got.Revision},
	} {
		_, err := svc.Delete(t.Context(), req, testAccount)
		testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
		assert.Equal(t, got.Revision, mustGet(t, svc, "hello").Revision, name)
		_, ok := deletion(t, svc, req.FunctionID)
		assert.False(t, ok, name)
	}
}

func TestDelete_RefusesMalformedRequests(t *testing.T) {
	svc, _ := newTestService(t)
	for name, req := range map[string]*DeleteRequest{
		"bad region": {Region: "Bad.Region", FunctionName: "hello", FunctionID: "f1", Revision: 1},
		"bad name":   {Region: testRegion, FunctionName: "a.b", FunctionID: "f1", Revision: 1},
		"bad id":     {Region: testRegion, FunctionName: "hello", FunctionID: "f.1", Revision: 1},
	} {
		_, err := svc.Delete(t.Context(), req, testAccount)
		require.Error(t, err, name)
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
}

// The delete passes its read, then pauses before writing while an update
// commits; the conditional delete loses and the function stays bound. The
// obligation it left proves nothing was deleted.
func TestDelete_InterleavedUpdateLosesTheConditionalDelete(t *testing.T) {
	svc, c := newTestService(t)
	got := serveAs(t, svc, "hello", "f1")
	paused, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	svc.now = func() time.Time {
		if calls.Add(1) == 1 {
			close(paused)
			<-release
		}
		return c.now()
	}
	deleteErr := make(chan error, 1)
	go func() {
		_, err := svc.Delete(t.Context(), deleteReq(got), testAccount)
		deleteErr <- err
	}()
	<-paused
	cfg := Configuration{Role: got.Record.Role, Handler: "handler.two", Timeout: 3, MemorySize: 128}
	won, err := svc.UpdateConfiguration(t.Context(), configReq(got.Revision, cfg), testAccount)
	require.NoError(t, err)
	close(release)
	testutil.RequireAWSCode(t, <-deleteErr, awserrors.ErrorResourceConflictException)

	after := mustGet(t, svc, "hello")
	require.True(t, after.Found)
	assert.Equal(t, won.Revision, after.Revision)
	assert.Equal(t, "f1", after.Record.FunctionID)
	_, ok := deletion(t, svc, "f1")
	assert.True(t, ok, "the obligation from the losing attempt is retained")
}

func TestDelete_ObligationNamingAnotherFunctionRefusesTheDelete(t *testing.T) {
	svc, _ := newTestService(t)
	got := serveAs(t, svc, "hello", "f1")
	kv, err := svc.bucket(t.Context(), testAccount)
	require.NoError(t, err)
	data, err := json.Marshal(Deletion{FormatVersion: FormatVersion, AccountID: testAccount, Region: testRegion, FunctionName: "other", FunctionID: "f1"})
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), KVDeletedKey(testRegion, "f1"), data)
	require.NoError(t, err)

	_, err = svc.Delete(t.Context(), deleteReq(got), testAccount)
	require.Error(t, err)
	_, coded := awserrors.ResolveErrorCode(err)
	assert.False(t, coded, "an inconsistent obligation is an internal fault, retried by the caller")
	assert.True(t, mustGet(t, svc, "hello").Found)
}

// A delete of a create still storing its code wins; the late CodeStored and
// RollbackCreate of that create change nothing, and its upload stays owed.
func TestDelete_PendingCreateRacingDeletionChangesNothingLater(t *testing.T) {
	svc, _ := newTestService(t)
	rev := mustCreate(t, svc, "hello", "f1")
	_, err := svc.Delete(t.Context(), &DeleteRequest{Region: testRegion, FunctionName: "hello", FunctionID: "f1", Revision: rev}, testAccount)
	require.NoError(t, err)
	recreated := serveAs(t, svc, "hello", "f2")

	_, err = svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: "hello", Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	require.Error(t, err)
	_, err = svc.RollbackCreate(t.Context(), &RollbackCreateRequest{Region: testRegion, FunctionName: "hello", Revision: rev}, testAccount)
	require.Error(t, err)

	got := mustGet(t, svc, "hello")
	assert.Equal(t, recreated.Revision, got.Revision)
	assert.Equal(t, "f2", got.Record.FunctionID)
	requireOwed(t, svc, "f1")
	requireRetained(t, svc, "hello", "f1")
}

func TestDelete_SameNameRecreationKeepsEachObligationApart(t *testing.T) {
	svc, _ := newTestService(t)
	first := serveAs(t, svc, "hello", "f1")
	_, err := svc.Delete(t.Context(), deleteReq(first), testAccount)
	require.NoError(t, err)
	second := serveAs(t, svc, "hello", "f2")
	_, ok := deletion(t, svc, "f2")
	assert.False(t, ok, "a recreated function has no obligation of its own")

	_, err = svc.Delete(t.Context(), deleteReq(second), testAccount)
	require.NoError(t, err)
	requireRetained(t, svc, "hello", "f1")
	requireRetained(t, svc, "hello", "f2")
}

// faultKV fails one write to key: before it commits, or after, losing the
// acknowledgement.
type faultKV struct {
	jetstream.KeyValue

	key   string
	after bool
	fired atomic.Bool
}

var errInjected = errors.New("nats: timeout")

func (f *faultKV) trip(key string) bool { return key == f.key && f.fired.CompareAndSwap(false, true) }

func (f *faultKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if !f.trip(key) {
		return f.KeyValue.Create(ctx, key, value, opts...)
	}
	if f.after {
		if _, err := f.KeyValue.Create(ctx, key, value, opts...); err != nil {
			return 0, err
		}
	}
	return 0, errInjected
}

func (f *faultKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if !f.trip(key) {
		return f.KeyValue.Delete(ctx, key, opts...)
	}
	if f.after {
		if err := f.KeyValue.Delete(ctx, key, opts...); err != nil {
			return err
		}
	}
	return errInjected
}

// Each write of a delete is interrupted before it commits, or commits with its
// acknowledgement lost. A fresh owner then finds the binding and obligation the
// stream actually holds, a retry converges, and a recreated name stays apart.
func TestDelete_InterruptionAndLostAcknowledgementAtEachWrite(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		after      bool
		bound      bool
		obligation bool
	}{
		{"obligation write interrupted", KVDeletedKey(testRegion, "f1"), false, true, false},
		{"obligation acknowledgement lost", KVDeletedKey(testRegion, "f1"), true, true, true},
		{"name delete interrupted", KVFunctionKey(testRegion, "hello"), false, true, true},
		{"name delete acknowledgement lost", KVFunctionKey(testRegion, "hello"), true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, nc, _ := testutil.StartTestJetStream(t)
			js := testutil.NewJetStream(t, nc)
			svc := NewMetaService(js)
			got := serveAs(t, svc, "hello", "f1")
			kv, err := svc.bucket(t.Context(), testAccount)
			require.NoError(t, err)
			svc.mu.Lock()
			svc.buckets[testAccount] = &faultKV{KeyValue: kv, key: tc.key, after: tc.after}
			svc.mu.Unlock()

			_, err = svc.Delete(t.Context(), deleteReq(got), testAccount)
			require.ErrorIs(t, err, errInjected)
			_, coded := awserrors.ResolveErrorCode(err)
			assert.False(t, coded, "an uncertain write is a retryable fault")

			reopened := NewMetaService(js)
			now := mustGet(t, reopened, "hello")
			assert.Equal(t, tc.bound, now.Found, "binding")
			if tc.bound {
				assert.Equal(t, got.Revision, now.Revision)
			}
			_, ok := deletion(t, reopened, "f1")
			assert.Equal(t, tc.obligation, ok, "obligation")
			_, ok = upload(t, reopened, "f1")
			assert.True(t, ok, "upload record retained")

			_, err = reopened.Delete(t.Context(), deleteReq(got), testAccount)
			if tc.bound {
				require.NoError(t, err, "the retry completes the delete")
			} else {
				testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
			}
			assert.False(t, mustGet(t, reopened, "hello").Found)
			requireRetained(t, reopened, "hello", "f1")

			recreated := serveAs(t, reopened, "hello", "f2")
			assert.Equal(t, "f2", recreated.Record.FunctionID)
			requireRetained(t, reopened, "hello", "f1")
			_, ok = deletion(t, reopened, "f2")
			assert.False(t, ok)
		})
	}
}
