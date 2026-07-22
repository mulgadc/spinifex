package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bedrockTestAccount = "123456789012"
const bedrockTestLlamaModelID = "meta.llama3-70b-instruct-v1:0"

// newVLLMStub stands up an httptest server that answers the OpenAI
// chat-completions wire, mirroring gateway/bedrock/vllm_test.go's stub.
func newVLLMStub(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi from vllm"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 4, "completion_tokens": 2}
		}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newLlamaCompletionsStub stands up an httptest server that answers the
// OpenAI completions wire, mirroring gateway/bedrock/invoke_llama_test.go's
// stub, for the InvokeModel self-host path.
func newLlamaCompletionsStub(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "hi from llama invoke", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 3}
		}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// grantAllModels is an AccessResolver that grants every model, standing in for
// a provisioned grant store in tests whose subject is routing rather than
// access control.
type grantAllModels struct{}

func (grantAllModels) Granted(_ context.Context, _, _ string) (bool, error) { return true, nil }

// newBedrockRequestGateway builds a GatewayConfig with a real NATS connection
// (satisfying Bedrock_Request/BedrockRuntime_Request's nil-check only — no
// NATS subject handling is required for these routes), no IAMService (so
// checkPolicy is a no-op), BedrockEndpoints pinned at the given vLLM stub, and
// every model granted — access is deny-by-default, so without a resolver these
// routes would all return before reaching the code under test.
func newBedrockRequestGateway(t *testing.T, vllmURL string) *GatewayConfig {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return &GatewayConfig{
		NATSConn:       nc,
		DisableLogging: true,
		BedrockEndpoints: map[string]string{
			bedrockTestLlamaModelID: vllmURL,
		},
		BedrockAccess: grantAllModels{},
	}
}

func bedrockRequestWithAccount(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxAccountID, bedrockTestAccount)
	return req.WithContext(ctx)
}

// TestBedrockRequest_DenyByDefault covers the whole gateway path with no
// access resolver configured, which is what a deployment that has granted
// nothing looks like: the catalog is empty and every runtime route refuses.
// This is the end-to-end guard on deny-by-default, one layer above the
// per-function access tests in gateway/bedrock.
func TestBedrockRequest_DenyByDefault(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)
	gw.BedrockAccess = nil // no grant store configured

	t.Run("list is empty", func(t *testing.T) {
		req := bedrockRequestWithAccount(http.MethodGet, "/foundation-models", "")
		w := httptest.NewRecorder()
		require.NoError(t, gw.Bedrock_Request(w, req))
		require.Equal(t, http.StatusOK, w.Code)

		var out bedrock.ListFoundationModelsOutput
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		assert.Empty(t, out.ModelSummaries)
	})

	t.Run("describe reports not found", func(t *testing.T) {
		req := bedrockRequestWithAccount(http.MethodGet, "/foundation-models/"+bedrockTestLlamaModelID, "")
		w := httptest.NewRecorder()
		err := gw.Bedrock_Request(w, req)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	})

	// Every runtime verb must refuse, not just the one that happens to be
	// checked first — guessing a modelId must gain nothing on any path.
	runtimeCases := []struct{ name, path, body string }{
		{"converse", "/model/" + bedrockTestLlamaModelID + "/converse", `{"messages":[]}`},
		{"invoke", "/model/" + bedrockTestLlamaModelID + "/invoke", `{"prompt":"hi"}`},
		{"converse-stream", "/model/" + bedrockTestLlamaModelID + "/converse-stream", `{"messages":[]}`},
		{"invoke-with-response-stream", "/model/" + bedrockTestLlamaModelID + "/invoke-with-response-stream", `{"prompt":"hi"}`},
	}
	for _, tc := range runtimeCases {
		t.Run(tc.name, func(t *testing.T) {
			req := bedrockRequestWithAccount(http.MethodPost, tc.path, tc.body)
			w := httptest.NewRecorder()
			err := gw.BedrockRuntime_Request(w, req)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
		})
	}
}

func TestBedrockRequest_ListFoundationModels(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	req := bedrockRequestWithAccount(http.MethodGet, "/foundation-models", "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.Bedrock_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrock.ListFoundationModelsOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))

	var ids []string
	for _, m := range out.ModelSummaries {
		ids = append(ids, *m.ModelId)
	}
	assert.Contains(t, ids, bedrockTestLlamaModelID)
}

func TestBedrockRequest_GetFoundationModel(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	req := bedrockRequestWithAccount(http.MethodGet, "/foundation-models/"+bedrockTestLlamaModelID, "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.Bedrock_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrock.GetFoundationModelOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.NotNil(t, out.ModelDetails)
	assert.Equal(t, bedrockTestLlamaModelID, *out.ModelDetails.ModelId)
}

func TestBedrockRequest_GetFoundationModel_NotFound(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	req := bedrockRequestWithAccount(http.MethodGet, "/foundation-models/does.not-exist-v1:0", "")
	w := httptest.NewRecorder()
	err := gw.Bedrock_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestBedrockRequest_UnknownRouteReturnsInvalidAction(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	req := bedrockRequestWithAccount(http.MethodDelete, "/foundation-models", "")
	w := httptest.NewRecorder()
	err := gw.Bedrock_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestBedrockRequest_MissingAccountIDReturnsServerInternal(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	req := httptest.NewRequest(http.MethodGet, "/foundation-models", nil)
	w := httptest.NewRecorder()
	err := gw.Bedrock_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestBedrockRuntimeRequest_Converse(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	body := `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`
	req := bedrockRequestWithAccount(http.MethodPost, "/model/"+bedrockTestLlamaModelID+"/converse", body)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockRuntime_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockruntime.ConverseOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.NotNil(t, out.Output)
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, bedrockruntime.ConversationRoleAssistant, *out.Output.Message.Role)
	require.Len(t, out.Output.Message.Content, 1)
	assert.Equal(t, "hi from vllm", *out.Output.Message.Content[0].Text)
}

func TestBedrockRuntimeRequest_MalformedBodyReturnsValidationException(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	req := bedrockRequestWithAccount(http.MethodPost, "/model/"+bedrockTestLlamaModelID+"/converse", "{not-json")
	w := httptest.NewRecorder()
	err := gw.BedrockRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

func TestBedrockRuntimeRequest_InvokeModel(t *testing.T) {
	ts := newLlamaCompletionsStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	body := `{"prompt":"hello","max_gen_len":128}`
	req := bedrockRequestWithAccount(http.MethodPost, "/model/"+bedrockTestLlamaModelID+"/invoke", body)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockRuntime_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	assert.JSONEq(t, `{
		"generation": "hi from llama invoke",
		"prompt_token_count": 5,
		"generation_token_count": 3,
		"stop_reason": "stop"
	}`, w.Body.String())
}

func TestBedrockRuntimeRequest_MissingAccountIDReturnsServerInternal(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	req := httptest.NewRequest(http.MethodPost, "/model/"+bedrockTestLlamaModelID+"/converse", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	err := gw.BedrockRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// newVLLMStreamStub stands up an httptest server answering the OpenAI
// chat-completions streaming (SSE) wire, for the ConverseStream self-host path.
func newVLLMStreamStub(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"hi from vllm stream"},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}

data: [DONE]

`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newLlamaCompletionsStreamStub stands up an httptest server answering the
// OpenAI completions streaming (SSE) wire, for the
// InvokeModelWithResponseStream self-host path.
func newLlamaCompletionsStreamStub(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"text":"hi from llama stream","finish_reason":null}]}

data: {"choices":[{"text":"","finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}

data: [DONE]

`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestBedrockRuntimeRequest_ConverseStream(t *testing.T) {
	ts := newVLLMStreamStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	body := `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`
	req := bedrockRequestWithAccount(http.MethodPost, "/model/"+bedrockTestLlamaModelID+"/converse-stream", body)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockRuntime_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/vnd.amazon.eventstream", w.Header().Get("Content-Type"))
	assert.NotZero(t, w.Body.Len())
}

func TestBedrockRuntimeRequest_ConverseStream_UnknownModelReturnsResourceNotFound(t *testing.T) {
	ts := newVLLMStreamStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	body := `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`
	req := bedrockRequestWithAccount(http.MethodPost, "/model/does.not-exist-v1:0/converse-stream", body)
	w := httptest.NewRecorder()
	err := gw.BedrockRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	// Pre-stream failure: BedrockRuntime_Request must not have written
	// anything, leaving ErrorHandler free to write the JSON envelope.
	assert.Zero(t, w.Body.Len())
}

func TestBedrockRuntimeRequest_InvokeModelWithResponseStream(t *testing.T) {
	ts := newLlamaCompletionsStreamStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	body := `{"prompt":"hello","max_gen_len":128}`
	req := bedrockRequestWithAccount(http.MethodPost, "/model/"+bedrockTestLlamaModelID+"/invoke-with-response-stream", body)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockRuntime_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/vnd.amazon.eventstream", w.Header().Get("Content-Type"))
	assert.NotZero(t, w.Body.Len())
}

func TestBedrockRuntimeRequest_InvokeModelWithResponseStream_UnknownModelReturnsResourceNotFound(t *testing.T) {
	ts := newLlamaCompletionsStreamStub(t)
	gw := newBedrockRequestGateway(t, ts.URL)

	body := `{"prompt":"hello"}`
	req := bedrockRequestWithAccount(http.MethodPost, "/model/does.not-exist-v1:0/invoke-with-response-stream", body)
	w := httptest.NewRecorder()
	err := gw.BedrockRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	assert.Zero(t, w.Body.Len())
}
