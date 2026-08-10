package gateway_bedrock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vllmStreamTestServer answers the vLLM (OpenAI chat-completions) SSE wire
// with vllmStreamFixture, standing in for a pinned self-host endpoint.
func vllmStreamTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// llamaStreamTestServer answers the Llama (OpenAI completions) SSE wire with
// llamaCompletionsStreamFixture, standing in for a pinned self-host endpoint.
func llamaStreamTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRouter_ConverseStream_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount
// is ConverseStream's counterpart to
// TestRouter_Converse_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount:
// a PT ARN on the streaming path must reach the same account-aware resolve a
// non-stream Converse call does, not silently fall through unresolved.
func TestRouter_ConverseStream_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount(t *testing.T) {
	ts := vllmStreamTestServer(t)
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := createPTCommitment(t, store, ptCallerAccount)

	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewRouter(nil, spy, nil, grantAll{}, store, nil)

	src, err := rt.ConverseStream(context.Background(), ptCallerAccount, arn, converseStreamInput())
	require.NoError(t, err)
	events := drainConverseStream(t, src)
	assert.NotEmpty(t, events)

	require.Len(t, spy.endpointForAccountCalls, 1)
	assert.Equal(t, ptCallerAccount, spy.endpointForAccountCalls[0].accountID,
		"the PT ARN's own account, not GlobalAccountID, must drive the resolve")
	assert.Equal(t, selfHostTestModel, spy.endpointForAccountCalls[0].modelID)
	assert.Empty(t, spy.endpointCalls, "a PT ARN must never fall back to the GlobalAccountID shorthand")
}

// TestRouter_ConverseStream_BareModelID_StillUsesGlobalShorthand is the
// regression guard: a bare modelId on the streaming path, even with a
// provisioned store configured, must keep resolving through the untouched
// Endpoint shorthand.
func TestRouter_ConverseStream_BareModelID_StillUsesGlobalShorthand(t *testing.T) {
	ts := vllmStreamTestServer(t)
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())

	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewRouter(nil, spy, nil, grantAll{}, store, nil)

	src, err := rt.ConverseStream(context.Background(), ptCallerAccount, selfHostTestModel, converseStreamInput())
	require.NoError(t, err)
	drainConverseStream(t, src)

	require.Len(t, spy.endpointCalls, 1)
	assert.Equal(t, selfHostTestModel, spy.endpointCalls[0])
	assert.Empty(t, spy.endpointForAccountCalls, "a bare modelId must never reach the account-aware resolve")
}

// TestRouter_ConverseStream_ProvisionedThroughputARN_UnknownCommitment covers
// a well-formed ARN whose commitment was never created (or was already
// deleted) on the streaming path.
func TestRouter_ConverseStream_ProvisionedThroughputARN_UnknownCommitment(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := FormatProvisionedModelARN(ptTestRegion, ptCallerAccount, "does-not-exist")

	rt := NewRouter(nil, &spyEndpointResolver{}, nil, grantAll{}, store, nil)
	_, err := rt.ConverseStream(context.Background(), ptCallerAccount, arn, converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestRouter_ConverseStream_ProvisionedThroughputARN_ForeignAccount is the
// cross-tenant isolation guard on the streaming path: a caller presenting
// another account's PT ARN must not resolve it, even though the commitment
// itself exists.
func TestRouter_ConverseStream_ProvisionedThroughputARN_ForeignAccount(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := createPTCommitment(t, store, ptCallerAccount)

	spy := &spyEndpointResolver{baseURL: "http://unused:8000"}
	rt := NewRouter(nil, spy, nil, grantAll{}, store, nil)

	_, err := rt.ConverseStream(context.Background(), ptOtherCaller, arn, converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	assert.Empty(t, spy.endpointForAccountCalls, "a foreign-account ARN must never reach endpoint resolution")
}

// TestInvokeStreamRouter_InvokeModelWithResponseStream_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount
// mirrors the ConverseStream coverage above for InvokeModelWithResponseStream's
// own resolve path (llamaInvokeAdapter's streaming side in invoke_llama.go).
func TestInvokeStreamRouter_InvokeModelWithResponseStream_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount(t *testing.T) {
	ts := llamaStreamTestServer(t)
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := createPTCommitment(t, store, ptCallerAccount)

	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewInvokeStreamRouter(nil, spy, grantAll{}, store)

	src, err := rt.InvokeModelWithResponseStream(context.Background(), ptCallerAccount, arn, []byte(`{"prompt":"hello"}`))
	require.NoError(t, err)
	chunks := drainInvokeStream(t, src)
	assert.NotEmpty(t, chunks)

	require.Len(t, spy.endpointForAccountCalls, 1)
	assert.Equal(t, ptCallerAccount, spy.endpointForAccountCalls[0].accountID)
	assert.Equal(t, selfHostTestModel, spy.endpointForAccountCalls[0].modelID)
	assert.Empty(t, spy.endpointCalls)
}

// TestInvokeStreamRouter_InvokeModelWithResponseStream_BareModelID_StillUsesGlobalShorthand
// is InvokeModelWithResponseStream's regression guard, mirroring the
// ConverseStream one.
func TestInvokeStreamRouter_InvokeModelWithResponseStream_BareModelID_StillUsesGlobalShorthand(t *testing.T) {
	ts := llamaStreamTestServer(t)
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())

	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewInvokeStreamRouter(nil, spy, grantAll{}, store)

	src, err := rt.InvokeModelWithResponseStream(context.Background(), ptCallerAccount, selfHostTestModel, []byte(`{"prompt":"hello"}`))
	require.NoError(t, err)
	drainInvokeStream(t, src)

	require.Len(t, spy.endpointCalls, 1)
	assert.Equal(t, selfHostTestModel, spy.endpointCalls[0])
	assert.Empty(t, spy.endpointForAccountCalls)
}

// TestInvokeStreamRouter_InvokeModelWithResponseStream_ProvisionedThroughputARN_UnknownCommitment
// covers a well-formed ARN whose commitment was never created, on the
// InvokeModelWithResponseStream path.
func TestInvokeStreamRouter_InvokeModelWithResponseStream_ProvisionedThroughputARN_UnknownCommitment(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := FormatProvisionedModelARN(ptTestRegion, ptCallerAccount, "does-not-exist")

	rt := NewInvokeStreamRouter(nil, &spyEndpointResolver{}, grantAll{}, store)
	_, err := rt.InvokeModelWithResponseStream(context.Background(), ptCallerAccount, arn, []byte(`{"prompt":"hello"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestInvokeStreamRouter_InvokeModelWithResponseStream_ProvisionedThroughputARN_ForeignAccount
// is the cross-tenant isolation guard for InvokeModelWithResponseStream: a
// caller presenting another account's PT ARN must not resolve it.
func TestInvokeStreamRouter_InvokeModelWithResponseStream_ProvisionedThroughputARN_ForeignAccount(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := createPTCommitment(t, store, ptCallerAccount)

	spy := &spyEndpointResolver{baseURL: "http://unused:8000"}
	rt := NewInvokeStreamRouter(nil, spy, grantAll{}, store)

	_, err := rt.InvokeModelWithResponseStream(context.Background(), ptOtherCaller, arn, []byte(`{"prompt":"hello"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	assert.Empty(t, spy.endpointForAccountCalls, "a foreign-account ARN must never reach endpoint resolution")
}
