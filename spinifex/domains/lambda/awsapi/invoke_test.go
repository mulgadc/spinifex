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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
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
	payload := []byte(`{"a":40,"b":2,"note":"caf\u00e9"}`)
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
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	assert.Equal(t, 0, ex.count())
}

func TestInvoke_FailsClosedWithoutAnExecutor(t *testing.T) {
	e, _ := newInvokeEnv(t)
	e.deps.Exec = nil
	_, err := e.invoke(t.Context(), invokePath, nil, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
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
	testutil.RequireAWSCode(t, err, awserrors.ErrorThrottling)

	ex.err = &awsapi.RefusalError{Err: errors.New("uncoded")}
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)

	ex.err = errors.New("transport lost")
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.Contains(t, err.Error()+errMessage(err), "outcome is unknown")

	ex.err = nil
	ex.result = awsapi.SyncResult{Payload: []byte(`{}`)}
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
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
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
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

// jsonOfSize is a JSON string exactly n bytes long.
func jsonOfSize(n int) []byte {
	return []byte(`"` + strings.Repeat("a", n-2) + `"`)
}

func TestInvoke_PayloadLimit(t *testing.T) {
	e, ex := newInvokeEnv(t)
	_, err := e.invoke(t.Context(), invokePath, nil, jsonOfSize(6*1024*1024))
	require.NoError(t, err)
	_, err = e.invoke(t.Context(), invokePath, nil, jsonOfSize(6*1024*1024+1))
	testutil.RequireAWSCode(t, err, awserrors.ErrorRequestEntityTooLargeException)
	assert.Equal(t, 1, ex.count())
}

func TestInvoke_PayloadMustBeJSONUnlessEmpty(t *testing.T) {
	e, ex := newInvokeEnv(t)
	for _, bad := range []string{"not json", `{"a":`, "{} {}", "\x00"} {
		_, err := e.invoke(t.Context(), invokePath, nil, []byte(bad))
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidRequestContentException)
	}
	assert.Equal(t, 0, ex.count(), "a malformed payload reached the executor")

	for _, good := range []string{"", `{"a":1}`, `[1,2]`, `"s"`, `42`, `null`, " true "} {
		_, err := e.invoke(t.Context(), invokePath, nil, []byte(good))
		require.NoError(t, err, "payload %q", good)
	}
	require.Equal(t, 7, ex.count())
	assert.Empty(t, ex.calls[0].Payload, "an empty body is dispatched as it is")
	assert.Equal(t, []byte(" true "), ex.calls[6].Payload)
}

func TestInvoke_MissingAndUnsupportedFunctions(t *testing.T) {
	e, ex := newInvokeEnv(t)
	_, err := e.invoke(t.Context(), "/2015-03-31/functions/ghost/invocations", nil, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, err = e.invoke(t.Context(), invokePath+"?Qualifier=1", nil, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	_, err = e.invoke(t.Context(), invokePath+"?Qualifier=%24LATEST", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, ex.count())
}

// Each form follows its own documented Invoke pattern; a documented value this
// prototype does not serve passes validation and fails closed at lookup.
func TestInvoke_QualifierFormsAndLengths(t *testing.T) {
	fn := "/2015-03-31/functions/"
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name, path, code string
	}{
		{"path $LATEST.PUBLISHED", fn + "hello:$LATEST.PUBLISHED/invocations", awserrors.ErrorResourceNotFoundException},
		{"query $LATEST.PUBLISHED", invokePath + "?Qualifier=%24LATEST.PUBLISHED", awserrors.ErrorResourceNotFoundException},
		{"query $foo", invokePath + "?Qualifier=%24foo", awserrors.ErrorResourceNotFoundException},
		{"query of 128 characters", invokePath + "?Qualifier=" + long(128), awserrors.ErrorResourceNotFoundException},
		{"query of 129 characters", invokePath + "?Qualifier=" + long(129), awserrors.ErrorInvalidParameterValueException},
		{"empty query", invokePath + "?Qualifier=", awserrors.ErrorInvalidParameterValueException},
		{"query with a dot", invokePath + "?Qualifier=a.b", awserrors.ErrorInvalidParameterValueException},
		{"query $LATEST.OTHER", invokePath + "?Qualifier=%24LATEST.OTHER", awserrors.ErrorInvalidParameterValueException},
		{"path $foo", fn + "hello:$foo/invocations", awserrors.ErrorInvalidParameterValueException},
		{"path with a dot", fn + "hello:a.b/invocations", awserrors.ErrorInvalidParameterValueException},
		{"function name of 256 characters", fn + "hello:" + long(250) + "/invocations", awserrors.ErrorResourceNotFoundException},
		{"function name of 257 characters", fn + "hello:" + long(251) + "/invocations", awserrors.ErrorInvalidParameterValueException},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ex := newInvokeEnv(t)
			_, err := e.invoke(t.Context(), tc.path, nil, nil)
			testutil.RequireAWSCode(t, err, tc.code)
			assert.Equal(t, 0, ex.count())
		})
	}
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
		return runPath(t, invokePath, statements...)
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
	t.Run("an unqualified call does not match a qualified-only allow", func(t *testing.T) {
		ex, err := run(t, gwtest.Statement("Allow", "lambda:InvokeFunction", arn+":$LATEST"))
		gwtest.AssertDenied(t, err)
		assert.Equal(t, 0, ex.count())
	})
	t.Run("an unqualified call does not match an allow on any qualified ARN", func(t *testing.T) {
		_, err := run(t, gwtest.Statement("Allow", "lambda:InvokeFunction", arn+":*"))
		gwtest.AssertDenied(t, err)
	})
	t.Run("an unqualified call matches a wildcard allow", func(t *testing.T) {
		ex, err := run(t, gwtest.Statement("Allow", "lambda:InvokeFunction", arn+"*"))
		require.NoError(t, err)
		assert.Equal(t, 1, ex.count())
	})

	// Lambda compares the policy resource with both the name's qualifier and
	// the Qualifier parameter, so both forms are checked the same way.
	qualified := map[string]string{
		"path qualifier":  "/2015-03-31/functions/hello:$LATEST/invocations",
		"query qualifier": invokePath + "?Qualifier=%24LATEST",
	}
	for form, path := range qualified {
		t.Run(form, func(t *testing.T) {
			t.Run("a qualified allow passes", func(t *testing.T) {
				ex, err := runPath(t, path, gwtest.Statement("Allow", "lambda:InvokeFunction", arn+":$LATEST"))
				require.NoError(t, err)
				assert.Equal(t, 1, ex.count())
			})
			t.Run("an explicit qualified deny overrides the matching qualified allow", func(t *testing.T) {
				ex, err := runPath(t, path,
					gwtest.Statement("Allow", "lambda:InvokeFunction", arn+":$LATEST"),
					gwtest.Statement("Deny", "lambda:InvokeFunction", arn+":$LATEST"))
				gwtest.AssertDenied(t, err)
				assert.Equal(t, 0, ex.count())
			})
			t.Run("an unqualified-only allow does not match", func(t *testing.T) {
				ex, err := runPath(t, path, gwtest.Statement("Allow", "lambda:InvokeFunction", arn))
				gwtest.AssertDenied(t, err)
				assert.Equal(t, 0, ex.count())
			})
			t.Run("an allow on any qualified ARN passes", func(t *testing.T) {
				ex, err := runPath(t, path, gwtest.Statement("Allow", "lambda:InvokeFunction", arn+":*"))
				require.NoError(t, err)
				assert.Equal(t, 1, ex.count())
			})
			t.Run("a wildcard allow passes", func(t *testing.T) {
				ex, err := runPath(t, path, gwtest.Statement("Allow", "lambda:InvokeFunction", arn+"*"))
				require.NoError(t, err)
				assert.Equal(t, 1, ex.count())
			})
		})
	}
}

func runPath(t *testing.T, path string, statements ...iamdomain.Statement) (*fakeExec, error) {
	t.Helper()
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
	_, err = e.invoke(t.Context(), path, nil, nil)
	return ex, err
}

// countingMeta counts metadata reads, to show a request never got that far.
type countingMeta struct {
	function.Client

	gets atomic.Int32
}

func (m *countingMeta) Get(ctx context.Context, account, region, name string) (function.Record, uint64, error) {
	m.gets.Add(1)
	return m.Client.Get(ctx, account, region, name)
}

// Every deterministic check runs before authorization, so a caller with no
// Invoke access still gets the validation error, and neither the metadata
// store nor the executor is reached. Whether AWS orders these the same way
// is not verified.
func TestInvoke_ValidationPrecedesAuthorization(t *testing.T) {
	big := append([]byte(`"`), bytes.Repeat([]byte("a"), 6*1024*1024)...)
	cases := []struct {
		name    string
		path    string
		header  http.Header
		payload []byte
		code    string
	}{
		{"unsupported invocation type", invokePath, http.Header{"X-Amz-Invocation-Type": {"Event"}}, nil, awserrors.ErrorInvalidParameterValueException},
		{"invalid invocation type", invokePath, http.Header{"X-Amz-Invocation-Type": {"Later"}}, nil, awserrors.ErrorInvalidParameterValueException},
		{"payload too large", invokePath, nil, append(big, '"'), awserrors.ErrorRequestEntityTooLargeException},
		{"payload not JSON", invokePath, nil, []byte(`{`), awserrors.ErrorInvalidRequestContentException},
		{"malformed function name", "/2015-03-31/functions/bad%20name/invocations", nil, nil, awserrors.ErrorInvalidParameterValueException},
		{"malformed path qualifier", "/2015-03-31/functions/hello:a.b/invocations", nil, nil, awserrors.ErrorInvalidParameterValueException},
		{"malformed query qualifier", invokePath + "?Qualifier=a.b", nil, nil, awserrors.ErrorInvalidParameterValueException},
		{"empty query qualifier", invokePath + "?Qualifier=", nil, nil, awserrors.ErrorInvalidParameterValueException},
		{"conflicting qualifiers", "/2015-03-31/functions/hello:$LATEST/invocations?Qualifier=2", nil, nil, awserrors.ErrorInvalidParameterValueException},
		{"malformed query encoding", invokePath + "?Qualifier=%ZZ", nil, nil, awserrors.ErrorInvalidParameterValueException},
		{"malformed pair beside a valid qualifier", invokePath + "?Qualifier=%24LATEST&x=%ZZ", nil, nil, awserrors.ErrorInvalidParameterValueException},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for access, statement := range map[string]iamdomain.Statement{
				"denied":  gwtest.Statement("Allow", "lambda:GetFunction", "*"),
				"allowed": gwtest.Statement("Allow", "lambda:InvokeFunction", "*"),
			} {
				e := newEnv(t, statement)
				meta := &countingMeta{Client: e.meta}
				e.deps.Meta = meta
				ex := &fakeExec{result: okResult}
				e.deps.Exec = ex
				_, err := e.invoke(t.Context(), tc.path, tc.header, tc.payload)
				testutil.RequireAWSCode(t, err, tc.code)
				assert.Zero(t, meta.gets.Load(), "%s: metadata was read", access)
				assert.Equal(t, 0, ex.count(), "%s: the executor ran", access)
			}
		})
	}
}
