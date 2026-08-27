package gateway_bedrock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCredentialResolver returns a fixed (key, ok, err) for every Resolve
// call, letting tests drive the Router's anthropic branch without a real
// CredentialStore.
type stubCredentialResolver struct {
	key string
	ok  bool
	err error
}

func (s stubCredentialResolver) Resolve(_ context.Context, _, _ string) (string, bool, error) {
	return s.key, s.ok, s.err
}

func converseInput() *bedrockruntime.ConverseInput {
	return &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}
}

func TestRouter_Converse_SelfHostSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	out, err := rt.Converse(context.Background(), "000000000001", modelID, converseInput())
	require.NoError(t, err)
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "hi", *out.Output.Message.Content[0].Text)
}

func TestRouter_Converse_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewRouter(nil, nil, nil, grantAll{}, nil, nil, nil)
	_, err := rt.Converse(context.Background(), "000000000001", "does.not-exist-v1:0", converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestRouter_Converse_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewRouter(stubCredentialResolver{ok: false}, nil, nil, grantAll{}, nil, nil, nil)
	_, err := rt.Converse(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestConverse_PackageEntryPoint_SelfHostSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "via package Converse"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	out, err := Converse(context.Background(), "000000000001", modelID, converseInput(), nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "via package Converse", *out.Output.Message.Content[0].Text)
}

func TestConverse_PackageEntryPoint_UnknownModel(t *testing.T) {
	_, err := Converse(context.Background(), "000000000001", "does.not-exist-v1:0", converseInput(), nil, nil, nil, grantAll{}, nil, nil, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func converseStreamInput() *bedrockruntime.ConverseStreamInput {
	return &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}
}

func TestRouter_ConverseStream_SelfHostSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	src, err := rt.ConverseStream(context.Background(), "000000000001", modelID, converseStreamInput())
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	events := drainConverseStream(t, src)
	assert.Equal(t, converseStreamEventMessageStart, events[0].Kind)
}

func TestRouter_ConverseStream_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewRouter(nil, nil, nil, grantAll{}, nil, nil, nil)
	_, err := rt.ConverseStream(context.Background(), "000000000001", "does.not-exist-v1:0", converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestRouter_ConverseStream_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewRouter(stubCredentialResolver{ok: false}, nil, nil, grantAll{}, nil, nil, nil)
	_, err := rt.ConverseStream(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestRouter_ConverseStream_SelfHostNoEndpointReturnsModelNotReady(t *testing.T) {
	rt := NewRouter(nil, nil, nil, grantAll{}, nil, nil, nil)
	_, err := rt.ConverseStream(context.Background(), "000000000001", "meta.llama3-2-1b-instruct-v1:0", converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

func TestNewRouter_NilArgumentsFallBackToNoops(t *testing.T) {
	rt := NewRouter(nil, nil, nil, grantAll{}, nil, nil, nil)
	require.NotNil(t, rt)

	// Self-host model with no endpoint resolver configured resolves nothing,
	// so it must report ModelNotReady rather than panic on a nil resolver.
	_, err := rt.Converse(context.Background(), "000000000001", "meta.llama3-2-1b-instruct-v1:0", converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

// TestRouter_Converse_SelfHostGatedAtCapacity proves the non-stream self-host
// path is admission-gated: pre-occupying the endpoint's only slot throttles
// the next Converse, and releasing it lets the following one through.
func TestRouter_Converse_SelfHostGatedAtCapacity(t *testing.T) {
	modelID := "self-host.throttle-converse-v1:0"
	withCatalogEntry(t, catalogEntry{ModelID: modelID, Provider: tierSelfHost, Family: familyMeta, MaxConcurrency: 1})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	// Occupy the endpoint's only admission slot directly, standing in for a
	// concurrent in-flight request on the same (account, model) key.
	release, ok := selfHostLimiter.Acquire(admissionKey("", modelID), 1)
	require.True(t, ok)

	_, err := rt.Converse(context.Background(), "000000000001", modelID, converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	release()

	out, err := rt.Converse(context.Background(), "000000000001", modelID, converseInput())
	require.NoError(t, err)
	require.NotNil(t, out.Output.Message)
}

// TestRouter_ConverseStream_SelfHostGatedAtCapacity mirrors the non-stream
// case for ConverseStream, and additionally proves the slot stays held for
// the whole stream lifetime — released only on the returned source's Close.
func TestRouter_ConverseStream_SelfHostGatedAtCapacity(t *testing.T) {
	modelID := "self-host.throttle-converse-stream-v1:0"
	withCatalogEntry(t, catalogEntry{ModelID: modelID, Provider: tierSelfHost, Family: familyMeta, MaxConcurrency: 1})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer ts.Close()

	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	release, ok := selfHostLimiter.Acquire(admissionKey("", modelID), 1)
	require.True(t, ok)

	_, err := rt.ConverseStream(context.Background(), "000000000001", modelID, converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	release()

	src, err := rt.ConverseStream(context.Background(), "000000000001", modelID, converseStreamInput())
	require.NoError(t, err)
	events := drainConverseStream(t, src)
	assert.Equal(t, converseStreamEventMessageStart, events[0].Kind)

	// The stream is drained but not yet closed: its own acquire still holds
	// the endpoint's only slot, so a further concurrent request must throttle.
	_, err = rt.ConverseStream(context.Background(), "000000000001", modelID, converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	require.NoError(t, src.Close())

	// Close released the slot; the endpoint is admissible again.
	src2, err := rt.ConverseStream(context.Background(), "000000000001", modelID, converseStreamInput())
	require.NoError(t, err)
	require.NoError(t, src2.Close())
}

// TestRouter_Converse_AnthropicPathIsNeverGated fires more concurrent
// Anthropic Converse calls than any self-host capacity in the catalog and
// asserts every one fails on the credential check (AccessDeniedException),
// never ThrottlingException — the managed-provider branch must never reach
// admitSelfHost.
func TestRouter_Converse_AnthropicPathIsNeverGated(t *testing.T) {
	rt := NewRouter(stubCredentialResolver{ok: false}, nil, nil, grantAll{}, nil, nil, nil)

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = rt.Converse(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", converseInput())
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
	}
}

// TestRouter_Converse_ProvisionedThroughputARN_CapacityMultipliesByModelUnits
// proves D2's capacity formula end to end: a PT ARN commitment with
// ModelUnits=3 against a MaxConcurrency=1 catalog entry admits exactly 3
// concurrent requests, not 1.
func TestRouter_Converse_ProvisionedThroughputARN_CapacityMultipliesByModelUnits(t *testing.T) {
	modelID := "self-host.throttle-pt-model-units-v1:0"
	withCatalogEntry(t, catalogEntry{ModelID: modelID, Provider: tierSelfHost, Family: familyMeta, MaxConcurrency: 1})

	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	out, err := CreateProvisionedModelThroughput(context.Background(), ptCallerAccount, store, createInput(modelID, "my-pt", 3))
	require.NoError(t, err)
	arn := aws.StringValue(out.ProvisionedModelArn)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, store, nil, nil)
	key := admissionKey(ptCallerAccount, modelID)

	// Occupy 1 of the 3 units directly (matching MaxConcurrency alone). If
	// the router only consulted MaxConcurrency and ignored ModelUnits, this
	// single occupied slot would already read as "at capacity" and the
	// Converse below would incorrectly throttle.
	release1, ok := selfHostLimiter.Acquire(key, 3)
	require.True(t, ok)

	out2, err := rt.Converse(context.Background(), ptCallerAccount, arn, converseInput())
	require.NoError(t, err, "capacity must be MaxConcurrency x ModelUnits (3), not MaxConcurrency alone (1)")
	require.NotNil(t, out2.Output.Message)

	// Occupy the remaining 2 units directly to reach the 3-unit ceiling —
	// Converse's own transient acquire/release above already gave back its
	// slot, so inFlight is back to 1 (just release1) at this point.
	release2, ok := selfHostLimiter.Acquire(key, 3)
	require.True(t, ok)
	release3, ok := selfHostLimiter.Acquire(key, 3)
	require.True(t, ok)

	_, err = rt.Converse(context.Background(), ptCallerAccount, arn, converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())

	release1()
	release2()
	release3()
}
