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
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
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
