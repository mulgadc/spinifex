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
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody))
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

// anthropicStreamFixture is a canned Anthropic Messages streaming SSE body
// covering the full taxonomy, including a keepalive ping and a
// tool_use content block to exercise structural passthrough.
const anthropicStreamFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":12}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicProvider_ConverseStream_MapsSSEToTaxonomy(t *testing.T) {
	var capturedBody anthropicRequest
	var capturedAPIKey string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAPIKey = r.Header.Get("X-Api-Key")
		require.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicStreamFixture))
	}))
	defer ts.Close()

	p := &anthropicProvider{httpClient: ts.Client(), baseURL: ts.URL}

	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}},
		},
	}

	src, err := p.ConverseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input, "sk-test-key")
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	assert.Equal(t, "sk-test-key", capturedAPIKey)
	assert.True(t, capturedBody.Stream)

	events := drainConverseStream(t, src)
	require.Len(t, events, 10)

	kinds := make([]converseStreamEventKind, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	assert.Equal(t, []converseStreamEventKind{
		converseStreamEventMessageStart,
		converseStreamEventContentBlockStart,
		converseStreamEventContentBlockDelta,
		converseStreamEventContentBlockDelta,
		converseStreamEventContentBlockStop,
		converseStreamEventContentBlockStart,
		converseStreamEventContentBlockDelta,
		converseStreamEventContentBlockStop,
		converseStreamEventMessageStop,
		converseStreamEventMetadata,
	}, kinds)

	assert.Equal(t, bedrockruntime.ConversationRoleAssistant, *events[0].MessageStart.Role)
	assert.Equal(t, "Hello", *events[2].ContentBlockDelta.Delta.Text)
	assert.Equal(t, " there", *events[3].ContentBlockDelta.Delta.Text)

	toolStart := events[5].ContentBlockStart
	require.NotNil(t, toolStart.Start.ToolUse)
	assert.Equal(t, "toolu_1", *toolStart.Start.ToolUse.ToolUseId)
	assert.Equal(t, "get_weather", *toolStart.Start.ToolUse.Name)

	toolDelta := events[6].ContentBlockDelta
	require.NotNil(t, toolDelta.Delta.ToolUse)
	assert.Equal(t, `{"city":`, *toolDelta.Delta.ToolUse.Input)

	assert.Equal(t, bedrockruntime.StopReasonToolUse, *events[8].MessageStop.StopReason)
	assert.Equal(t, int64(12), *events[9].Metadata.Usage.InputTokens)
	assert.Equal(t, int64(5), *events[9].Metadata.Usage.OutputTokens)
	assert.Equal(t, int64(17), *events[9].Metadata.Usage.TotalTokens)
}

func TestAnthropicConverseStreamSource_MessageStopEmitsMetadata(t *testing.T) {
	raw := `event: message_start
data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":4}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(raw))
	}))
	defer ts.Close()

	p := &anthropicProvider{httpClient: ts.Client(), baseURL: ts.URL}
	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}}},
	}
	src, err := p.ConverseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input, "k")
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	events := drainConverseStream(t, src)
	require.Len(t, events, 3)
	assert.Equal(t, converseStreamEventMetadata, events[2].Kind)
	assert.Equal(t, int64(4), *events[2].Metadata.Usage.InputTokens)
	assert.Equal(t, int64(2), *events[2].Metadata.Usage.OutputTokens)
	assert.Equal(t, int64(6), *events[2].Metadata.Usage.TotalTokens)
}

func TestAnthropicConverseStreamSource_ErrorEventSurfacesAsStreamFault(t *testing.T) {
	raw := "event: error\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(raw))
	}))
	defer ts.Close()

	p := &anthropicProvider{httpClient: ts.Client(), baseURL: ts.URL}
	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}}},
	}
	src, err := p.ConverseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input, "k")
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	_, ok, err := src.Next(context.Background())
	assert.False(t, ok)
	require.Error(t, err)
	var fault *streamFaultError
	assert.ErrorAs(t, err, &fault)
	assert.Contains(t, err.Error(), "overloaded")
}

func TestAnthropicProvider_ConverseStream_401ReturnsAccessDenied(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "invalid x-api-key"}}`))
	}))
	defer ts.Close()

	p := &anthropicProvider{httpClient: ts.Client(), baseURL: ts.URL}
	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}}},
	}
	_, err := p.ConverseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input, "bad-key")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
}

func TestAnthropicBoundProvider_ConverseStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "sk-bound-test", r.Header.Get("X-Api-Key"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicStreamFixture))
	}))
	defer ts.Close()

	p := newAnthropicProvider("sk-bound-test")
	bound, ok := p.(*anthropicBoundProvider)
	require.True(t, ok)
	bound.inner.httpClient = ts.Client()
	bound.inner.baseURL = ts.URL

	sp, ok := p.(ConverseStreamProvider)
	require.True(t, ok)

	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}}},
	}
	src, err := sp.ConverseStream(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", input)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	events := drainConverseStream(t, src)
	require.NotEmpty(t, events)
	assert.Equal(t, converseStreamEventMessageStart, events[0].Kind)
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
