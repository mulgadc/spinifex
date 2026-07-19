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

func TestAnthropicInvokeAdapter_InvokeModel_RewritesRequestAndReturnsBodyVerbatim(t *testing.T) {
	canned := `{"id":"msg_123","content":[{"type":"text","text":"Hello there"}],"stop_reason":"end_turn"}`

	var captured map[string]json.RawMessage
	var capturedAPIKey, capturedAnthropicVersion, capturedContentType string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAPIKey = r.Header.Get("X-Api-Key")
		capturedAnthropicVersion = r.Header.Get("Anthropic-Version")
		capturedContentType = r.Header.Get("Content-Type")
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(canned))
	}))
	defer ts.Close()

	a := &anthropicInvokeAdapter{httpClient: ts.Client(), baseURL: ts.URL}

	reqBody := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	respBody, contentType, err := a.InvokeModel(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", reqBody, "sk-test-key")
	require.NoError(t, err)

	assert.Equal(t, "sk-test-key", capturedAPIKey)
	assert.Equal(t, anthropicAPIVersion, capturedAnthropicVersion)
	assert.Equal(t, "application/json", capturedContentType)

	_, hasVersion := captured["anthropic_version"]
	assert.False(t, hasVersion, "anthropic_version must be dropped from the forwarded request")

	var model string
	require.NoError(t, json.Unmarshal(captured["model"], &model))
	assert.Equal(t, "claude-3-5-sonnet-20240620", model)

	require.Contains(t, captured, "max_tokens")
	require.Contains(t, captured, "messages")

	assert.Equal(t, "application/json", contentType)
	assert.JSONEq(t, canned, string(respBody))
}

func TestAnthropicInvokeAdapter_InvokeModel_401ReturnsAccessDenied(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "invalid x-api-key"}}`))
	}))
	defer ts.Close()

	a := &anthropicInvokeAdapter{httpClient: ts.Client(), baseURL: ts.URL}

	_, _, err := a.InvokeModel(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`), "bad-key")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

// TestBoundAnthropicInvokeAdapter_InvokeModel exercises newAnthropicInvokeAdapter
// and boundAnthropicInvokeAdapter.InvokeModel (the InvokeAdapter used by
// InvokeRouter for the "provider:anthropic" tier). newAnthropicInvokeAdapter
// hardcodes the real Anthropic base URL, so — same package as production code
// — the test reaches into the unexported inner client to redirect it at an
// httptest stub instead of the network.
func TestAnthropicInvokeAdapter_InvokeModelWithResponseStream_ForwardsSSEVerbatim(t *testing.T) {
	const raw = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_stop
data: {"type":"message_stop"}

`
	var captured map[string]json.RawMessage

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(raw))
	}))
	defer ts.Close()

	a := &anthropicInvokeAdapter{httpClient: ts.Client(), baseURL: ts.URL}
	reqBody := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	src, err := a.InvokeModelWithResponseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", reqBody, "sk-test-key")
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	var streamField bool
	require.NoError(t, json.Unmarshal(captured["stream"], &streamField))
	assert.True(t, streamField)
	_, hasVersion := captured["anthropic_version"]
	assert.False(t, hasVersion)

	chunks := drainInvokeStream(t, src)
	require.Len(t, chunks, 3, "ping is skipped; message_start, content_block_delta, message_stop forward verbatim")
	assert.JSONEq(t, `{"type":"message_start","message":{"id":"msg_1"}}`, string(chunks[0]))
	assert.JSONEq(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, string(chunks[1]))
	assert.JSONEq(t, `{"type":"message_stop"}`, string(chunks[2]))
}

func TestAnthropicInvokeAdapter_InvokeModelWithResponseStream_401ReturnsAccessDenied(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "invalid x-api-key"}}`))
	}))
	defer ts.Close()

	a := &anthropicInvokeAdapter{httpClient: ts.Client(), baseURL: ts.URL}
	_, err := a.InvokeModelWithResponseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`), "bad-key")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestBoundAnthropicInvokeAdapter_InvokeModelWithResponseStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "sk-bound-test", r.Header.Get("X-Api-Key"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer ts.Close()

	adapter := newAnthropicInvokeAdapter("sk-bound-test")
	bound, ok := adapter.(*boundAnthropicInvokeAdapter)
	require.True(t, ok)
	bound.inner.httpClient = ts.Client()
	bound.inner.baseURL = ts.URL

	sa, ok := adapter.(InvokeStreamAdapter)
	require.True(t, ok)

	src, err := sa.InvokeModelWithResponseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`))
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	chunks := drainInvokeStream(t, src)
	require.Len(t, chunks, 1)
	assert.JSONEq(t, `{"type":"message_stop"}`, string(chunks[0]))
}

func TestBoundAnthropicInvokeAdapter_InvokeModel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "sk-bound-test", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"bound response"}],"stop_reason":"end_turn"}`))
	}))
	defer ts.Close()

	adapter := newAnthropicInvokeAdapter("sk-bound-test")
	bound, ok := adapter.(*boundAnthropicInvokeAdapter)
	require.True(t, ok)
	require.NotNil(t, bound.inner)
	bound.inner.httpClient = ts.Client()
	bound.inner.baseURL = ts.URL

	respBody, contentType, err := adapter.InvokeModel(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`))
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)
	assert.Contains(t, string(respBody), "bound response")
}
