package gateway_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnthropicProvider_Converse_MapsResponse(t *testing.T) {
	var capturedBody anthropicRequest
	var capturedAPIKey string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAPIKey = r.Header.Get("X-Api-Key")
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "Hello there"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 12, "output_tokens": 5}
		}`))
	}))
	defer ts.Close()

	p := &anthropicProvider{httpClient: ts.Client(), baseURL: ts.URL}

	input := &bedrockruntime.ConverseInput{
		System: []*bedrockruntime.SystemContentBlock{{Text: aws.String("be nice")}},
		Messages: []*bedrockruntime.Message{
			{
				Role:    aws.String(bedrockruntime.ConversationRoleUser),
				Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}},
			},
		},
		InferenceConfig: &bedrockruntime.InferenceConfiguration{
			MaxTokens: aws.Int64(256),
		},
	}

	out, err := p.Converse(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input, "sk-test-key")
	require.NoError(t, err)

	assert.Equal(t, "sk-test-key", capturedAPIKey)
	assert.Equal(t, "claude-3-5-sonnet-20240620", capturedBody.Model)
	assert.Equal(t, int64(256), capturedBody.MaxTokens)
	assert.Equal(t, "be nice", capturedBody.System)
	require.Len(t, capturedBody.Messages, 1)
	assert.Equal(t, "user", capturedBody.Messages[0].Role)

	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "assistant", *out.Output.Message.Role)
	require.Len(t, out.Output.Message.Content, 1)
	assert.Equal(t, "Hello there", *out.Output.Message.Content[0].Text)
	assert.Equal(t, int64(12), *out.Usage.InputTokens)
	assert.Equal(t, int64(5), *out.Usage.OutputTokens)
	assert.Equal(t, int64(17), *out.Usage.TotalTokens)
	assert.Equal(t, bedrockruntime.StopReasonEndTurn, *out.StopReason)
	require.NotNil(t, out.Metrics)
	assert.GreaterOrEqual(t, *out.Metrics.LatencyMs, int64(0))
}

func TestAnthropicProvider_Converse_401ReturnsAccessDenied(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "invalid x-api-key"}}`))
	}))
	defer ts.Close()

	p := &anthropicProvider{httpClient: ts.Client(), baseURL: ts.URL}

	input := &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}},
		},
	}

	_, err := p.Converse(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input, "bad-key")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

// TestAnthropicBoundProvider_Converse exercises newAnthropicProvider,
// newAnthropicProviderClient, and anthropicBoundProvider.Converse (the
// Provider used by Router for the "provider:anthropic" tier). The real
// Anthropic base URL is hardcoded by newAnthropicProvider, so — same package
// as production code — the test reaches into the unexported inner client to
// redirect it at an httptest stub instead of the network.
func TestAnthropicBoundProvider_Converse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "sk-bound-test", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "bound response"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 3, "output_tokens": 2}
		}`))
	}))
	defer ts.Close()

	p := newAnthropicProvider("sk-bound-test")
	bound, ok := p.(*anthropicBoundProvider)
	require.True(t, ok)
	require.NotNil(t, bound.inner)
	bound.inner.httpClient = ts.Client()
	bound.inner.baseURL = ts.URL

	input := &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}},
		},
	}

	out, err := p.Converse(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input)
	require.NoError(t, err)
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "bound response", *out.Output.Message.Content[0].Text)
}

func TestAnthropicModelID(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"sonnet with version suffix", "anthropic.claude-3-5-sonnet-20240620-v1:0", "claude-3-5-sonnet-20240620"},
		{"haiku with version suffix", "anthropic.claude-3-haiku-20240307-v1:0", "claude-3-haiku-20240307"},
		{"non-matching id passes through", "meta.llama3-70b-instruct-v1:0", "meta.llama3-70b-instruct-v1:0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, anthropicModelID(tc.in))
		})
	}
}
