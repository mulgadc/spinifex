package awsapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeExec struct {
	mu     sync.Mutex
	calls  []awsapi.SyncInvocation
	ctxs   []context.Context
	result awsapi.SyncResult
	err    error
	// started, when set, is closed once the call begins and the call then
	// blocks until release is closed.
	started, release chan struct{}
}

func (f *fakeExec) InvokeSync(ctx context.Context, inv awsapi.SyncInvocation) (awsapi.SyncResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inv)
	f.ctxs = append(f.ctxs, ctx)
	started, release := f.started, f.release
	f.mu.Unlock()
	if started != nil {
		close(started)
		<-release
	}
	return f.result, f.err
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var okResult = awsapi.SyncResult{Payload: []byte(`{"sum":42}`), ExecutedVersion: "$LATEST"}

// newInvokeEnv creates "hello" with its code stored and reports it Active.
func newInvokeEnv(t *testing.T, statements ...iamdomain.Statement) (*env, *fakeExec) {
	t.Helper()
	e := newEnv(t, statements...)
	_, err := e.do(http.MethodPost, "/2015-03-31/functions", createBody("hello", func(b map[string]any) {
		b["Environment"] = map[string]any{"Variables": map[string]string{"MODE": "prod"}}
	}))
	require.NoError(t, err)
	e.meta.forceActive = true
	ex := &fakeExec{result: okResult}
	e.deps.Exec = ex
	return e, ex
}

func (e *env) invoke(ctx context.Context, path string, header http.Header, payload []byte) (*httptest.ResponseRecorder, error) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(payload))
	maps.Copy(req.Header, header)
	err := e.h.Dispatch(awsapi.NewRegistration(e.deps), awsapi.ServiceName, rec, req)
	return rec, err
}

const invokePath = "/2015-03-31/functions/hello/invocations"

func TestInvoke_ReturnsTheExecutorPayloadAndHeaders(t *testing.T) {
	e, ex := newInvokeEnv(t)
	payload := []byte("not json \x00 bytes")
	rec, err := e.invoke(t.Context(), invokePath, nil, payload)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"sum":42}`, rec.Body.String())
	assert.Equal(t, "$LATEST", rec.Header().Get("X-Amz-Executed-Version"))
	assert.NotContains(t, rec.Header(), "X-Amz-Function-Error")
	assert.NotContains(t, rec.Header(), "X-Amz-Log-Result")

	require.Equal(t, 1, ex.count())
	got := ex.calls[0]
	assert.Equal(t, payload, got.Payload)
	stored, revision, err := e.meta.Get(t.Context(), gwtest.AccountID, gwtest.Region, "hello")
	require.NoError(t, err)
	assert.Equal(t, revision, got.Revision)
	assert.Equal(t, stored, got.Function)
	assert.NotEmpty(t, got.Function.EnvCipher)
	assert.NotContains(t, got.Function.EnvCipher, "prod")
}

func TestInvoke_FunctionErrorHeaderComesFromTheExecutor(t *testing.T) {
	e, ex := newInvokeEnv(t)
	ex.result = awsapi.SyncResult{Payload: []byte(`{"errorType":"x"}`), FunctionError: "Unhandled", ExecutedVersion: "$LATEST"}
	rec, err := e.invoke(t.Context(), invokePath, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Unhandled", rec.Header().Get("X-Amz-Function-Error"))
	assert.Equal(t, `{"errorType":"x"}`, rec.Body.String())
}

func TestInvoke_PendingFunctionIsAConflictAndNeverDispatches(t *testing.T) {
	e, ex := newInvokeEnv(t)
	e.meta.forceActive = false
	_, err := e.invoke(t.Context(), invokePath, nil, nil)
	requireCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, 0, ex.count())
}

func TestInvoke_FailsClosedWithoutAnExecutor(t *testing.T) {
	e, _ := newInvokeEnv(t)
	e.deps.Exec = nil
	_, err := e.invoke(t.Context(), invokePath, nil, nil)
	requireCode(t, err, awserrors.ErrorServiceException)
}

func TestInvoke_DoesNotNeedTheCodeStore(t *testing.T) {
	e, _ := newInvokeEnv(t)
	e.deps.Codes = func(context.Context) (awsapi.CodeStore, error) { return nil, errors.New("code store down") }
	_, err := e.invoke(t.Context(), invokePath, nil, nil)
	require.NoError(t, err)
	e.deps.Codes = nil
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	require.NoError(t, err)
}

func TestInvoke_RefusalBeforeDispatchIsKeptApartFromAnUnknownOutcome(t *testing.T) {
	e, ex := newInvokeEnv(t)

	ex.err = &awsapi.RefusalError{Err: awserrors.Errorf(awserrors.ErrorThrottling, "Rate exceeded")}
	_, err := e.invoke(t.Context(), invokePath, nil, nil)
	requireCode(t, err, awserrors.ErrorThrottling)

	ex.err = &awsapi.RefusalError{Err: errors.New("uncoded")}
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	requireCode(t, err, awserrors.ErrorServiceException)

	ex.err = errors.New("transport lost")
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	requireCode(t, err, awserrors.ErrorServiceException)
	assert.Contains(t, err.Error()+errMessage(err), "outcome is unknown")

	ex.err = nil
	ex.result = awsapi.SyncResult{Payload: []byte(`{}`)}
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	requireCode(t, err, awserrors.ErrorServiceException)
}

func errMessage(err error) string {
	_, msg, _ := awserrors.ResolveErrorDetail(err)
	return msg
}

func TestInvoke_RefusesOptionsItDoesNotImplement(t *testing.T) {
	e, ex := newInvokeEnv(t)
	for _, h := range []http.Header{
		{"X-Amz-Invocation-Type": {"Event"}},
		{"X-Amz-Invocation-Type": {"DryRun"}},
		{"X-Amz-Invocation-Type": {"Bogus"}},
		{"X-Amz-Log-Type": {"Tail"}},
		{"X-Amz-Log-Type": {"Bogus"}},
		{"X-Amz-Client-Context": {"e30="}},
		{"X-Amz-Tenant-Id": {"t1"}},
	} {
		_, err := e.invoke(t.Context(), invokePath, h, nil)
		requireCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
	assert.Equal(t, 0, ex.count())

	for _, h := range []http.Header{
		{"X-Amz-Invocation-Type": {"RequestResponse"}},
		{"X-Amz-Log-Type": {"None"}},
	} {
		_, err := e.invoke(t.Context(), invokePath, h, nil)
		require.NoError(t, err)
	}
}

func TestInvoke_PayloadLimit(t *testing.T) {
	e, ex := newInvokeEnv(t)
	_, err := e.invoke(t.Context(), invokePath, nil, make([]byte, 6*1024*1024))
	require.NoError(t, err)
	_, err = e.invoke(t.Context(), invokePath, nil, make([]byte, 6*1024*1024+1))
	requireCode(t, err, awserrors.ErrorRequestEntityTooLarge)
	assert.Equal(t, 1, ex.count())
}

func TestInvoke_MissingAndUnsupportedFunctions(t *testing.T) {
	e, ex := newInvokeEnv(t)
	_, err := e.invoke(t.Context(), "/2015-03-31/functions/ghost/invocations", nil, nil)
	requireCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, err = e.invoke(t.Context(), invokePath+"?Qualifier=1", nil, nil)
	requireCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, err = e.invoke(t.Context(), invokePath+"?Qualifier=%24LATEST", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, ex.count())
}

func TestInvoke_CancelledBeforeDispatchNeverReachesTheExecutor(t *testing.T) {
	e, ex := newInvokeEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.invoke(ctx, invokePath, nil, nil)
	require.Error(t, err)
	assert.Equal(t, 0, ex.count())
}

func TestInvoke_CallerDisconnectDoesNotCancelAnAcceptedAttempt(t *testing.T) {
	e, ex := newInvokeEnv(t)
	ex.started, ex.release = make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := e.invoke(ctx, invokePath, nil, nil)
		done <- err
	}()
	<-ex.started
	cancel()

	actx := ex.ctxs[0]
	assert.NoError(t, actx.Err(), "the caller's disconnect reached the attempt")
	deadline, ok := actx.Deadline()
	require.True(t, ok, "the wait must be bounded")
	assert.Equal(t, ex.calls[0].WaitUntil, deadline)
	assert.WithinDuration(t, time.Now().Add(3*time.Second+60*time.Second), deadline, 5*time.Second)

	close(ex.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after the attempt finished")
	}
}

func TestInvoke_Authorization(t *testing.T) {
	arn := "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
	run := func(t *testing.T, statements ...iamdomain.Statement) (*fakeExec, error) {
		e := newEnv(t, statements...)
		sum := sha256.Sum256(testZip)
		rev, err := e.meta.Create(t.Context(), gwtest.AccountID, gwtest.Region, function.Record{
			FunctionID: "f1", FunctionName: "hello", Role: roleARN, Runtime: "provided.al2023", Handler: "h",
			Timeout: 3, MemorySize: 128, Code: function.Code{RevisionID: "r1"},
		})
		require.NoError(t, err)
		_, _, err = e.meta.CodeStored(t.Context(), gwtest.AccountID, gwtest.Region, "hello", rev, hex.EncodeToString(sum[:]), int64(len(testZip)))
		require.NoError(t, err)
		e.meta.forceActive = true
		ex := &fakeExec{result: okResult}
		e.deps.Exec = ex
		_, err = e.invoke(t.Context(), invokePath, nil, nil)
		return ex, err
	}

	t.Run("lambda:GetFunction alone is not enough", func(t *testing.T) {
		ex, err := run(t, gwtest.Statement("Allow", "lambda:GetFunction", "*"))
		gwtest.AssertDenied(t, err)
		assert.Equal(t, 0, ex.count())
	})
	t.Run("a deny on the function ARN applies", func(t *testing.T) {
		ex, err := run(t,
			gwtest.Statement("Allow", "lambda:*", "*"),
			gwtest.Statement("Deny", "lambda:InvokeFunction", arn))
		gwtest.AssertDenied(t, err)
		assert.Equal(t, 0, ex.count())
	})
	t.Run("an allow on another function ARN does not apply", func(t *testing.T) {
		_, err := run(t, gwtest.Statement("Allow", "lambda:InvokeFunction", arn+"-other"))
		gwtest.AssertDenied(t, err)
	})
	t.Run("an allow of lambda:InvokeFunction on the function ARN passes", func(t *testing.T) {
		ex, err := run(t, gwtest.Statement("Allow", "lambda:InvokeFunction", arn))
		require.NoError(t, err)
		assert.Equal(t, 1, ex.count())
	})
}
