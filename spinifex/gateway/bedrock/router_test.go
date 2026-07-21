package gateway_bedrock

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	modelID := "meta.llama3-70b-instruct-v1:0"
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))

	out, err := rt.Converse(context.Background(), "000000000001", modelID, converseInput())
	require.NoError(t, err)
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "hi", *out.Output.Message.Content[0].Text)
}

func TestRouter_Converse_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewRouter(nil, nil)
	_, err := rt.Converse(context.Background(), "000000000001", "does.not-exist-v1:0", converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestRouter_Converse_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewRouter(stubCredentialResolver{ok: false}, nil)
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

	modelID := "meta.llama3-70b-instruct-v1:0"
	out, err := Converse(context.Background(), "000000000001", modelID, converseInput(), nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	require.NoError(t, err)
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "via package Converse", *out.Output.Message.Content[0].Text)
}

func TestConverse_PackageEntryPoint_UnknownModel(t *testing.T) {
	_, err := Converse(context.Background(), "000000000001", "does.not-exist-v1:0", converseInput(), nil, nil)
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

	modelID := "meta.llama3-70b-instruct-v1:0"
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))

	src, err := rt.ConverseStream(context.Background(), "000000000001", modelID, converseStreamInput())
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	events := drainConverseStream(t, src)
	assert.Equal(t, converseStreamEventMessageStart, events[0].Kind)
}

func TestRouter_ConverseStream_UnknownModelReturnsResourceNotFound(t *testing.T) {
	rt := NewRouter(nil, nil)
	_, err := rt.ConverseStream(context.Background(), "000000000001", "does.not-exist-v1:0", converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
}

func TestRouter_ConverseStream_AnthropicNoCredentialReturnsAccessDenied(t *testing.T) {
	rt := NewRouter(stubCredentialResolver{ok: false}, nil)
	_, err := rt.ConverseStream(context.Background(), "000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0", converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestRouter_ConverseStream_SelfHostNoEndpointReturnsModelNotReady(t *testing.T) {
	rt := NewRouter(nil, nil)
	_, err := rt.ConverseStream(context.Background(), "000000000001", "meta.llama3-70b-instruct-v1:0", converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

func TestNewRouter_NilArgumentsFallBackToNoops(t *testing.T) {
	rt := NewRouter(nil, nil)
	require.NotNil(t, rt)

	// Self-host model with no endpoint resolver configured resolves nothing,
	// so it must report ModelNotReady rather than panic on a nil resolver.
	_, err := rt.Converse(context.Background(), "000000000001", "meta.llama3-70b-instruct-v1:0", converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}
