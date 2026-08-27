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

// errEndpointResolver returns a caller-supplied non-nil err from every
// resolve call, driving the endpoint-resolution failure branch a real daemon
// outage or admission refusal would trigger.
type errEndpointResolver struct {
	err error
}

var _ EndpointResolver = errEndpointResolver{}

func (r errEndpointResolver) Endpoint(_ context.Context, _ string) (string, bool, error) {
	return "", false, r.err
}

func (r errEndpointResolver) EndpointForAccount(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, r.err
}

// withCatalogEntry appends entry to the package-level catalog for the
// duration of the test, restoring the original slice on cleanup — the
// dispatch tests need a self-host entry outside the shipped family set.
func withCatalogEntry(t *testing.T, entry catalogEntry) {
	t.Helper()
	original := catalog
	catalog = append(append([]catalogEntry{}, catalog...), entry)
	t.Cleanup(func() { catalog = original })
}

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

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	respBody, contentType, err := rt.InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "hi", out.Generation)
}

func TestInvokeRouter_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewInvokeRouter(nil, nil, nil, grantAll{}, nil, nil, nil)
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", "does.not-exist-v1:0", []byte(`{}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestInvokeRouter_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewInvokeRouter(stubCredentialResolver{ok: false}, nil, nil, grantAll{}, nil, nil, nil)
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestInvokeRouter_SelfHostNoEndpointReturnsModelNotReady(t *testing.T) {
	rt := NewInvokeRouter(nil, nil, nil, grantAll{}, nil, nil, nil)
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", "meta.llama3-2-1b-instruct-v1:0", []byte(`{"prompt":"hello"}`), "", "")
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

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	respBody, contentType, err := InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, "", "", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "via package InvokeModel", out.Generation)
}

func TestInvokeModel_PackageEntryPoint_UnknownModel(t *testing.T) {
	_, _, err := InvokeModel(context.Background(), "000000000001", "does.not-exist-v1:0", []byte(`{}`), nil, nil, nil, grantAll{}, nil, "", "", nil, nil)
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

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	rt := NewInvokeStreamRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, nil, nil, nil)

	src, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	chunks := drainInvokeStream(t, src)
	assert.Len(t, chunks, 3)
}

func TestInvokeStreamRouter_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewInvokeStreamRouter(nil, nil, grantAll{}, nil, nil, nil)
	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", "does.not-exist-v1:0", []byte(`{}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestInvokeStreamRouter_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewInvokeStreamRouter(stubCredentialResolver{ok: false}, nil, grantAll{}, nil, nil, nil)
	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", []byte(`{}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestInvokeStreamRouter_SelfHostNoEndpointReturnsModelNotReady(t *testing.T) {
	rt := NewInvokeStreamRouter(nil, nil, grantAll{}, nil, nil, nil)
	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", "meta.llama3-2-1b-instruct-v1:0", []byte(`{"prompt":"hello"}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

// TestInvokeRouter_SelfHostUnhandledFamilyReturnsValidationException proves a
// self-host entry outside the shipped family set is refused outright rather
// than mis-served through Llama's native request schema — the fake endpoint
// server records whether it was ever called at all.
func TestInvokeRouter_SelfHostUnhandledFamilyReturnsValidationException(t *testing.T) {
	modelID := "self-host.unhandled-family-v1:0"
	withCatalogEntry(t, catalogEntry{ModelID: modelID, Provider: tierSelfHost, Family: "mistral"})

	var called bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
	assert.False(t, called, "an unhandled self-host family must never reach the Llama-serving endpoint")
}

// TestInvokeStreamRouter_SelfHostUnhandledFamilyReturnsValidationException
// mirrors TestInvokeRouter_SelfHostUnhandledFamilyReturnsValidationException
// for the streaming router.
func TestInvokeStreamRouter_SelfHostUnhandledFamilyReturnsValidationException(t *testing.T) {
	modelID := "self-host.unhandled-family-v1:0"
	withCatalogEntry(t, catalogEntry{ModelID: modelID, Provider: tierSelfHost, Family: "mistral"})

	var called bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	rt := NewInvokeStreamRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, nil, nil, nil)
	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
	assert.False(t, called, "an unhandled self-host family must never reach the Llama-serving endpoint")
}

// TestInvokeRouter_SelfHostGatedAtCapacity proves InvokeModel's self-host
// path shares the same admission gate as Converse: pre-occupying the
// endpoint's only slot throttles the next call, and releasing it admits
// the following one.
func TestInvokeRouter_SelfHostGatedAtCapacity(t *testing.T) {
	modelID := "self-host.throttle-invoke-v1:0"
	withCatalogEntry(t, catalogEntry{ModelID: modelID, Provider: tierSelfHost, Family: familyMeta, MaxConcurrency: 1})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "hi", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	release, ok := selfHostLimiter.Acquire(admissionKey("", modelID), 1)
	require.True(t, ok)

	_, _, err := rt.InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	release()

	respBody, _, err := rt.InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "hi", out.Generation)
}

// TestInvokeStreamRouter_SelfHostGatedAtCapacity mirrors the non-stream case
// for InvokeModelWithResponseStream, and proves the slot stays held for the
// whole stream lifetime — released only on the returned source's Close.
func TestInvokeStreamRouter_SelfHostGatedAtCapacity(t *testing.T) {
	modelID := "self-host.throttle-invoke-stream-v1:0"
	withCatalogEntry(t, catalogEntry{ModelID: modelID, Provider: tierSelfHost, Family: familyMeta, MaxConcurrency: 1})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	defer ts.Close()

	rt := NewInvokeStreamRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, nil, nil, nil)

	release, ok := selfHostLimiter.Acquire(admissionKey("", modelID), 1)
	require.True(t, ok)

	_, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	release()

	src, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)
	chunks := drainInvokeStream(t, src)
	assert.NotEmpty(t, chunks)

	// The stream is drained but not yet closed: its own acquire still holds
	// the endpoint's only slot, so a further concurrent request must throttle.
	_, err = rt.InvokeModelWithResponseStream(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	require.NoError(t, src.Close())

	// Close released the slot; the endpoint is admissible again.
	src2, err := rt.InvokeModelWithResponseStream(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)
	require.NoError(t, src2.Close())
}
