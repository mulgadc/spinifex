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

func TestVLLMProvider_Converse_MapsResponse(t *testing.T) {
	var captured vllmChatRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi there"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 3}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	input := &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
		InferenceConfig: &bedrockruntime.InferenceConfiguration{MaxTokens: aws.Int64(128)},
	}

	out, err := p.Converse(context.Background(), modelID, input)
	require.NoError(t, err)

	assert.Equal(t, modelID, captured.Model)
	require.NotNil(t, captured.MaxTokens)
	assert.Equal(t, int64(128), *captured.MaxTokens)
	require.Len(t, captured.Messages, 1)
	assert.Equal(t, "user", captured.Messages[0].Role)
	assert.Equal(t, "hello", captured.Messages[0].Content)

	require.NotNil(t, out.Output.Message)
	require.Len(t, out.Output.Message.Content, 1)
	assert.Equal(t, "hi there", *out.Output.Message.Content[0].Text)
	assert.Equal(t, int64(8), *out.Usage.InputTokens)
	assert.Equal(t, int64(3), *out.Usage.OutputTokens)
	assert.Equal(t, int64(11), *out.Usage.TotalTokens)
	assert.Equal(t, bedrockruntime.StopReasonEndTurn, *out.StopReason)
}

func TestVLLMProvider_Converse_UnresolvedEndpointReturnsModelNotReady(t *testing.T) {
	p := newVLLMProvider(NewStaticEndpointResolver(nil))

	input := &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}

	_, err := p.Converse(context.Background(), "meta.llama3-70b-instruct-v1:0", input)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

// vllmStreamFixture is a canned OpenAI chat-completions streaming SSE body:
// a role-only first chunk, two content deltas, a finish_reason chunk, a
// trailing usage-only chunk (stream_options.include_usage), then [DONE].
const vllmStreamFixture = `data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":" world"},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3}}

data: [DONE]

`

// drainConverseStream reads src to clean EOF, returning the ordered events.
func drainConverseStream(t *testing.T, src converseStreamSource) []ConverseStreamEvent {
	t.Helper()
	var events []ConverseStreamEvent
	for {
		ev, ok, err := src.Next(context.Background())
		require.NoError(t, err)
		if !ok {
			return events
		}
		events = append(events, ev)
	}
}

func TestVLLMProvider_ConverseStream_MapsSSEToTaxonomy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		var req vllmChatRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.True(t, req.Stream)
		require.NotNil(t, req.StreamOptions)
		assert.True(t, req.StreamOptions.IncludeUsage)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}

	src, err := p.ConverseStream(context.Background(), modelID, input)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	events := drainConverseStream(t, src)
	require.Len(t, events, 7)

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
		converseStreamEventMessageStop,
		converseStreamEventMetadata,
	}, kinds)

	assert.Equal(t, bedrockruntime.ConversationRoleAssistant, *events[0].MessageStart.Role)
	assert.Equal(t, int64(0), *events[1].ContentBlockStart.ContentBlockIndex)
	assert.Equal(t, "Hello", *events[2].ContentBlockDelta.Delta.Text)
	assert.Equal(t, " world", *events[3].ContentBlockDelta.Delta.Text)
	assert.Equal(t, int64(0), *events[4].ContentBlockStop.ContentBlockIndex)
	assert.Equal(t, bedrockruntime.StopReasonEndTurn, *events[5].MessageStop.StopReason)
	assert.Equal(t, int64(8), *events[6].Metadata.Usage.InputTokens)
	assert.Equal(t, int64(3), *events[6].Metadata.Usage.OutputTokens)
	assert.Equal(t, int64(11), *events[6].Metadata.Usage.TotalTokens)
	assert.GreaterOrEqual(t, *events[6].Metadata.Metrics.LatencyMs, int64(0))
}

func TestVLLMProvider_ConverseStream_UnresolvedEndpointReturnsModelNotReady(t *testing.T) {
	p := newVLLMProvider(NewStaticEndpointResolver(nil))

	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}

	_, err := p.ConverseStream(context.Background(), "meta.llama3-70b-instruct-v1:0", input)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}

func TestVLLMProvider_ConverseStream_UpstreamNon2xxMapsStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}

	_, err := p.ConverseStream(context.Background(), modelID, input)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())
}

// A usage-only chunk that arrives without a preceding finish_reason chunk must
// still yield contentBlockStop+messageStop before metadata, not metadata first.
func TestVLLMProvider_ConverseStream_UsageWithoutFinishReasonKeepsOrder(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	src, err := p.ConverseStream(context.Background(), modelID, &bedrockruntime.ConverseStreamInput{})
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	events := drainConverseStream(t, src)
	kinds := make([]converseStreamEventKind, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	assert.Equal(t, []converseStreamEventKind{
		converseStreamEventMessageStart,
		converseStreamEventContentBlockStart,
		converseStreamEventContentBlockDelta,
		converseStreamEventContentBlockStop,
		converseStreamEventMessageStop,
		converseStreamEventMetadata,
	}, kinds)
}

// A zero-content stream (upstream closes before any choice chunk) must still be
// well-formed: messageStart before the synthesized stop events, never orphaned.
func TestVLLMProvider_ConverseStream_EmptyStreamIsWellFormed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	src, err := p.ConverseStream(context.Background(), modelID, &bedrockruntime.ConverseStreamInput{})
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	events := drainConverseStream(t, src)
	require.NotEmpty(t, events)
	assert.Equal(t, converseStreamEventMessageStart, events[0].Kind)
	kinds := make([]converseStreamEventKind, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	assert.Equal(t, []converseStreamEventKind{
		converseStreamEventMessageStart,
		converseStreamEventContentBlockStart,
		converseStreamEventContentBlockStop,
		converseStreamEventMessageStop,
		converseStreamEventMetadata,
	}, kinds)
}

func TestVLLMConverseStreamSource_MidStreamDecodeErrorSurfacesAsStreamFault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {not-json\n\n"))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	input := &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}

	src, err := p.ConverseStream(context.Background(), modelID, input)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	_, ok, err := src.Next(context.Background())
	assert.False(t, ok)
	require.Error(t, err)
	var fault *streamFaultError
	assert.ErrorAs(t, err, &fault)
}
