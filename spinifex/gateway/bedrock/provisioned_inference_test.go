package gateway_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endpointForAccountCall is one recorded EndpointForAccount invocation.
type endpointForAccountCall struct {
	accountID string
	modelID   string
}

// spyEndpointResolver records every Endpoint/EndpointForAccount call so a
// test can assert which resolve path the PT-ARN translation actually drove
// — the whole point of decision (A) — while answering a fixed base URL
// either way.
type spyEndpointResolver struct {
	mu      sync.Mutex
	baseURL string

	endpointCalls           []string
	endpointForAccountCalls []endpointForAccountCall
}

var _ EndpointResolver = (*spyEndpointResolver)(nil)

func (s *spyEndpointResolver) Endpoint(_ context.Context, modelID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpointCalls = append(s.endpointCalls, modelID)
	return s.baseURL, s.baseURL != "", nil
}

func (s *spyEndpointResolver) EndpointForAccount(_ context.Context, accountID, modelID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpointForAccountCalls = append(s.endpointForAccountCalls, endpointForAccountCall{accountID: accountID, modelID: modelID})
	return s.baseURL, s.baseURL != "", nil
}

// vllmChatFixture is a minimal successful OpenAI chat-completions response,
// used by every PT-ARN Converse test below: the point of these tests is
// which endpoint got resolved, not the response translation itself.
const vllmChatFixture = `{
	"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 1, "completion_tokens": 1}
}`

func vllmTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmChatFixture))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// createPTCommitment creates a self-host PT commitment for accountID and
// returns its ARN, standing in for a prior CreateProvisionedModelThroughput
// call the inference path only ever consumes.
func createPTCommitment(t *testing.T, store *ProvisionedStore, accountID string) string {
	t.Helper()
	out, err := CreateProvisionedModelThroughput(context.Background(), accountID, store,
		createInput(selfHostTestModel, "my-pt", 1))
	require.NoError(t, err)
	return aws.StringValue(out.ProvisionedModelArn)
}

// TestRouter_Converse_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount
// is decision (A)'s core assertion: a PT ARN on the inference path must
// reach the account-aware resolve keyed on the commitment's own account, not
// the GlobalAccountID shorthand every bare-modelId caller uses.
func TestRouter_Converse_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount(t *testing.T) {
	ts := vllmTestServer(t)
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := createPTCommitment(t, store, ptCallerAccount)

	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewRouter(nil, spy, nil, grantAll{}, store, nil)

	out, err := rt.Converse(context.Background(), ptCallerAccount, arn, converseInput())
	require.NoError(t, err)
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "hi", *out.Output.Message.Content[0].Text)

	require.Len(t, spy.endpointForAccountCalls, 1)
	assert.Equal(t, ptCallerAccount, spy.endpointForAccountCalls[0].accountID,
		"the PT ARN's own account, not GlobalAccountID, must drive the resolve")
	assert.Equal(t, selfHostTestModel, spy.endpointForAccountCalls[0].modelID)
	assert.Empty(t, spy.endpointCalls, "a PT ARN must never fall back to the GlobalAccountID shorthand")
}

// TestRouter_Converse_BareModelID_StillUsesGlobalShorthand is the regression
// guard: a bare modelId, even with a provisioned store configured, must keep
// resolving through the untouched Endpoint shorthand.
func TestRouter_Converse_BareModelID_StillUsesGlobalShorthand(t *testing.T) {
	ts := vllmTestServer(t)
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())

	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewRouter(nil, spy, nil, grantAll{}, store, nil)

	_, err := rt.Converse(context.Background(), ptCallerAccount, selfHostTestModel, converseInput())
	require.NoError(t, err)

	require.Len(t, spy.endpointCalls, 1)
	assert.Equal(t, selfHostTestModel, spy.endpointCalls[0])
	assert.Empty(t, spy.endpointForAccountCalls, "a bare modelId must never reach the account-aware resolve")
}

// TestRouter_Converse_ProvisionedThroughputARN_UnknownCommitment covers a
// well-formed ARN whose commitment was never created (or was already
// deleted): it must read exactly like any other unresolvable model
// identifier, not a distinct error shape.
func TestRouter_Converse_ProvisionedThroughputARN_UnknownCommitment(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := FormatProvisionedModelARN(ptTestRegion, ptCallerAccount, "does-not-exist")

	rt := NewRouter(nil, &spyEndpointResolver{}, nil, grantAll{}, store, nil)
	_, err := rt.Converse(context.Background(), ptCallerAccount, arn, converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

// TestRouter_Converse_ProvisionedThroughputARN_ForeignAccount is the
// cross-tenant isolation guard: a caller presenting another account's PT ARN
// must not resolve it, even though the commitment itself exists.
func TestRouter_Converse_ProvisionedThroughputARN_ForeignAccount(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := createPTCommitment(t, store, ptCallerAccount)

	spy := &spyEndpointResolver{baseURL: "http://unused:8000"}
	rt := NewRouter(nil, spy, nil, grantAll{}, store, nil)

	_, err := rt.Converse(context.Background(), ptOtherCaller, arn, converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	assert.Empty(t, spy.endpointForAccountCalls, "a foreign-account ARN must never reach endpoint resolution")
}

// TestInvokeRouter_InvokeModel_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount
// mirrors the Converse coverage above for InvokeModel's own resolve path
// (llamaInvokeAdapter in invoke_llama.go), confirming both entry points
// share the same translate-before-resolve behaviour.
func TestInvokeRouter_InvokeModel_ProvisionedThroughputARN_ResolvesPinnedEndpointForRecordAccount(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "hi", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	arn := createPTCommitment(t, store, ptCallerAccount)

	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewInvokeRouter(nil, spy, nil, grantAll{}, store, nil)

	respBody, contentType, err := rt.InvokeModel(context.Background(), ptCallerAccount, arn, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "hi", out.Generation)

	require.Len(t, spy.endpointForAccountCalls, 1)
	assert.Equal(t, ptCallerAccount, spy.endpointForAccountCalls[0].accountID)
	assert.Equal(t, selfHostTestModel, spy.endpointForAccountCalls[0].modelID)
	assert.Empty(t, spy.endpointCalls)
}

// TestInvokeRouter_InvokeModel_BareModelID_StillUsesGlobalShorthand is
// InvokeModel's regression guard, mirroring the Converse one.
func TestInvokeRouter_InvokeModel_BareModelID_StillUsesGlobalShorthand(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices": [{"text": "hi", "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer ts.Close()

	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	spy := &spyEndpointResolver{baseURL: ts.URL}
	rt := NewInvokeRouter(nil, spy, nil, grantAll{}, store, nil)

	_, _, err := rt.InvokeModel(context.Background(), ptCallerAccount, selfHostTestModel, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)

	require.Len(t, spy.endpointCalls, 1)
	assert.Equal(t, selfHostTestModel, spy.endpointCalls[0])
	assert.Empty(t, spy.endpointForAccountCalls)
}
