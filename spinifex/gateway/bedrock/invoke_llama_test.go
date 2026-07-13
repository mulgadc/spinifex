package gateway_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLlamaInvokeAdapter_InvokeModel_MapsRequestAndResponse(t *testing.T) {
	var captured llamaCompletionsRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "hi there", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 3}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	a := newLlamaInvokeAdapter(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	a.httpClient = ts.Client()

	temp := 0.5
	reqBody, err := json.Marshal(llamaInvokeRequest{Prompt: "hello", MaxGenLen: 128, Temperature: &temp})
	require.NoError(t, err)

	respBody, contentType, err := a.InvokeModel(context.Background(), modelID, reqBody)
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)

	assert.Equal(t, modelID, captured.Model)
	assert.Equal(t, "hello", captured.Prompt)
	assert.Equal(t, 128, captured.MaxTokens)
	require.NotNil(t, captured.Temperature)
	assert.Equal(t, 0.5, *captured.Temperature)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "hi there", out.Generation)
	assert.Equal(t, 8, out.PromptTokenCount)
	assert.Equal(t, 3, out.GenerationTokenCount)
	assert.Equal(t, "stop", out.StopReason)
}

func TestLlamaInvokeAdapter_InvokeModel_MapsLengthFinishReason(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "truncated", "finish_reason": "length"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	a := newLlamaInvokeAdapter(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	a.httpClient = ts.Client()

	respBody, _, err := a.InvokeModel(context.Background(), modelID, []byte(`{"prompt":"hello"}`))
	require.NoError(t, err)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "length", out.StopReason)
}

func TestLlamaInvokeAdapter_InvokeModel_EmptyChoicesReturnsModelError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices": [], "usage": {"prompt_tokens": 1, "completion_tokens": 0}}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	a := newLlamaInvokeAdapter(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	a.httpClient = ts.Client()

	_, _, err := a.InvokeModel(context.Background(), modelID, []byte(`{"prompt":"hello"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
}

func TestLlamaInvokeAdapter_InvokeModel_UnresolvedEndpointReturnsModelNotReady(t *testing.T) {
	a := newLlamaInvokeAdapter(NewStaticEndpointResolver(nil))

	_, _, err := a.InvokeModel(context.Background(), "meta.llama3-70b-instruct-v1:0", []byte(`{"prompt":"hello"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

// llamaCompletionsStreamFixture is a canned OpenAI /v1/completions streaming
// SSE body: two text deltas, a finish_reason chunk, a trailing usage-only
// chunk, then [DONE].
const llamaCompletionsStreamFixture = `data: {"choices":[{"text":"Hello","finish_reason":null}]}

data: {"choices":[{"text":" world","finish_reason":null}]}

data: {"choices":[{"text":"","finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2}}

data: [DONE]

`

func drainInvokeStream(t *testing.T, src invokeStreamSource) [][]byte {
	t.Helper()
	var chunks [][]byte
	for {
		chunk, ok, err := src.Next(context.Background())
		require.NoError(t, err)
		if !ok {
			return chunks
		}
		chunks = append(chunks, chunk)
	}
}

func TestLlamaInvokeAdapter_InvokeModelWithResponseStream_TranslatesChunks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llamaCompletionsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.True(t, req.Stream)
		require.NotNil(t, req.StreamOptions)
		assert.True(t, req.StreamOptions.IncludeUsage)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	a := newLlamaInvokeAdapter(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	a.httpClient = ts.Client()

	src, err := a.InvokeModelWithResponseStream(context.Background(), modelID, []byte(`{"prompt":"hello","max_gen_len":128}`))
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	chunks := drainInvokeStream(t, src)
	require.Len(t, chunks, 3)

	var c1, c2 llamaInvokeStreamChunk
	require.NoError(t, json.Unmarshal(chunks[0], &c1))
	assert.Equal(t, "Hello", c1.Generation)
	require.NoError(t, json.Unmarshal(chunks[1], &c2))
	assert.Equal(t, " world", c2.Generation)

	var final llamaInvokeStreamFinalChunk
	require.NoError(t, json.Unmarshal(chunks[2], &final))
	assert.Equal(t, "", final.Generation)
	assert.Equal(t, "stop", final.StopReason)
	assert.Equal(t, 6, final.PromptTokenCount)
	assert.Equal(t, 2, final.GenerationTokenCount)
	assert.Equal(t, 6, final.InvocationMetrics.InputTokenCount)
	assert.Equal(t, 2, final.InvocationMetrics.OutputTokenCount)
	assert.GreaterOrEqual(t, final.InvocationMetrics.InvocationLatency, int64(0))
}

func TestLlamaInvokeAdapter_InvokeModelWithResponseStream_UnresolvedEndpointReturnsModelNotReady(t *testing.T) {
	a := newLlamaInvokeAdapter(NewStaticEndpointResolver(nil))

	_, err := a.InvokeModelWithResponseStream(context.Background(), "meta.llama3-70b-instruct-v1:0", []byte(`{"prompt":"hello"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

func TestLlamaInvokeStreamSource_MidStreamDecodeErrorSurfacesAsStreamFault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {not-json\n\n"))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	a := newLlamaInvokeAdapter(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	a.httpClient = ts.Client()

	src, err := a.InvokeModelWithResponseStream(context.Background(), modelID, []byte(`{"prompt":"hello"}`))
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	_, ok, err := src.Next(context.Background())
	assert.False(t, ok)
	require.Error(t, err)
	var fault *streamFaultError
	assert.ErrorAs(t, err, &fault)
}
