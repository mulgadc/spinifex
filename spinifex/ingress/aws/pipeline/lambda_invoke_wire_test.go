package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// activeMeta serves one Active function "hello" with its code stored.
type activeMeta struct{}

var _ function.Client = activeMeta{}

func (activeMeta) Get(_ context.Context, account, region, name string) (function.Record, uint64, error) {
	if name != "hello" {
		return function.Record{}, 0, function.ErrNotFound
	}
	return function.Record{
		FunctionID: "f1", AccountID: account, FunctionName: name, Runtime: "provided.al2023",
		Timeout: 3, MemorySize: 128, State: function.StateActive,
		Code: function.Code{RevisionID: "r1", State: function.CodeStored},
	}, 1, nil
}

func (activeMeta) Create(context.Context, string, string, function.Record) (uint64, error) {
	return 0, errors.New("unused")
}

func (activeMeta) CodeStored(context.Context, string, string, string, uint64, string, int64) (function.Record, uint64, error) {
	return function.Record{}, 0, errors.New("unused")
}

func (activeMeta) RollbackCreate(context.Context, string, string, string, uint64) error {
	return errors.New("unused")
}

// countingExec records how many invocations reached it.
type countingExec struct{ calls atomic.Int32 }

func (e *countingExec) InvokeSync(context.Context, lambdaawsapi.SyncInvocation) (lambdaawsapi.SyncResult, error) {
	e.calls.Add(1)
	return lambdaawsapi.SyncResult{Payload: []byte(`{"ok":true}`), ExecutedVersion: "$LATEST"}, nil
}

func signedInvoke(t *testing.T, app http.Handler, body []byte) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/hello/invocations", bytes.NewReader(body))
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, lambdaawsapi.ServiceName, time.Now().UTC())
	return doRequest(app, req)
}

// paddedJSON is a valid JSON string exactly n bytes long.
func paddedJSON(n int) []byte {
	return []byte(`"` + strings.Repeat("a", n-2) + `"`)
}

// restJSONError decodes the Lambda REST-JSON error body: Type and message,
// with the code carried only in X-Amzn-Errortype.
func restJSONError(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	var out map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "User", out["Type"])
	assert.NotEmpty(t, out["message"])
	return out
}

func lambdaInvokeApp(t *testing.T) (http.Handler, *countingExec) {
	t.Helper()
	ex := &countingExec{}
	_, app := dispatchSeamApp(t, allowAllECRDocs(), lambdaawsapi.NewRegistration(lambdaawsapi.Deps{Meta: activeMeta{}, Exec: ex}))
	return app, ex
}

func TestLambdaInvokeWire_PayloadSizeLimit(t *testing.T) {
	app, ex := lambdaInvokeApp(t)

	resp := signedInvoke(t, app, paddedJSON(6291456))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), ex.calls.Load())

	for _, size := range []int{6291457, 6291556, 7000000} {
		resp := signedInvoke(t, app, paddedJSON(size))
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, "size %d", size)
		assert.Equal(t, "RequestEntityTooLargeException", resp.Header.Get("X-Amzn-Errortype"), "size %d", size)
		body := restJSONError(t, resp)
		if size == 7000000 {
			assert.Equal(t, "Request must be smaller than 6291456 bytes for the InvokeFunction operation", body["message"])
		}
	}
	assert.Equal(t, int32(1), ex.calls.Load(), "an oversized payload reached the executor")
}

func TestLambdaInvokeWire_MalformedPayload(t *testing.T) {
	app, ex := lambdaInvokeApp(t)

	resp := signedInvoke(t, app, []byte("not json"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "InvalidRequestContentException", resp.Header.Get("X-Amzn-Errortype"))
	restJSONError(t, resp)
	assert.Equal(t, int32(0), ex.calls.Load(), "a malformed payload reached the executor")

	for _, good := range []string{`{"a":1}`, `[1]`, `"s"`, `7`, `null`} {
		resp := signedInvoke(t, app, []byte(good))
		assert.Equal(t, http.StatusOK, resp.StatusCode, "payload %s", good)
	}
	assert.Equal(t, int32(5), ex.calls.Load())
}
