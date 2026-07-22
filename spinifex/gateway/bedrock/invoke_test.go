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

func TestInvokeRouter_SelfHostSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "hi", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{})

	respBody, contentType, err := rt.InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`))
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "hi", out.Generation)
}

func TestInvokeRouter_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewInvokeRouter(nil, nil, grantAll{})
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", "does.not-exist-v1:0", []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestInvokeRouter_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewInvokeRouter(stubCredentialResolver{ok: false}, nil, grantAll{})
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestInvokeRouter_SelfHostNoEndpointReturnsModelNotReady(t *testing.T) {
	rt := NewInvokeRouter(nil, nil, grantAll{})
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", "meta.llama3-70b-instruct-v1:0", []byte(`{"prompt":"hello"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

func TestInvokeModel_PackageEntryPoint_SelfHostSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "via package InvokeModel", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	respBody, contentType, err := InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{})
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "via package InvokeModel", out.Generation)
}

func TestInvokeModel_PackageEntryPoint_UnknownModel(t *testing.T) {
	_, _, err := InvokeModel(context.Background(), "000000000001", "does.not-exist-v1:0", []byte(`{}`), nil, nil, grantAll{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestInvokeStreamRouter_SelfHostSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	rt := NewInvokeStreamRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{})

	src, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`))
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	chunks := drainInvokeStream(t, src)
	assert.Len(t, chunks, 3)
}

func TestInvokeStreamRouter_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewInvokeStreamRouter(nil, nil, grantAll{})
	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", "does.not-exist-v1:0", []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestInvokeStreamRouter_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewInvokeStreamRouter(stubCredentialResolver{ok: false}, nil, grantAll{})
	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestInvokeStreamRouter_SelfHostNoEndpointReturnsModelNotReady(t *testing.T) {
	rt := NewInvokeStreamRouter(nil, nil, grantAll{})
	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", "meta.llama3-70b-instruct-v1:0", []byte(`{"prompt":"hello"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}
