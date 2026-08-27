package gateway_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingVLLMServer returns an httptest.Server that answers every request
// with a fixed non-streaming vLLM chat-completions response carrying content,
// and a counter of how many requests it actually received — the "backend NOT
// called" assertion for an INPUT-blocked guardrail.
func countingVLLMServer(t *testing.T, content string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "` + content + `"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// guardedConverseInput builds a ConverseInput carrying one user message and a
// GuardrailConfig addressing guardrailID's DRAFT.
func guardedConverseInput(text string, guardrailID *string, trace bool) *bedrockruntime.ConverseInput {
	cfg := &bedrockruntime.GuardrailConfiguration{
		GuardrailIdentifier: guardrailID,
		GuardrailVersion:    aws.String(guardrailDraftVersion),
	}
	if trace {
		cfg.Trace = aws.String(bedrockruntime.GuardrailTraceEnabled)
	}
	return &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String(text)}}},
		},
		GuardrailConfig: cfg,
	}
}

func TestRouter_Converse_GuardrailInputBlock_SkipsBackend(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-input-block"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, hits := countingVLLMServer(t, "hi")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	out, err := rt.Converse(ctx, grCallerAccount, modelID, guardedConverseInput("this has a badword in it", createOut.GuardrailId, false))
	require.NoError(t, err)

	assert.Equal(t, int32(0), hits.Load(), "the backend must never be called on an INPUT block")
	require.NotNil(t, out.Output.Message)
	require.Len(t, out.Output.Message.Content, 1)
	assert.Equal(t, "Your input violates our policy.", aws.StringValue(out.Output.Message.Content[0].Text))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, aws.StringValue(out.StopReason))
	assert.Equal(t, int64(0), aws.Int64Value(out.Usage.InputTokens))
	assert.Nil(t, out.Trace)
}

func TestRouter_Converse_GuardrailOutputBlock(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-output-block"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, _ := countingVLLMServer(t, "this has a badword in it")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	out, err := rt.Converse(ctx, grCallerAccount, modelID, guardedConverseInput("hello", createOut.GuardrailId, false))
	require.NoError(t, err)

	require.NotNil(t, out.Output.Message)
	require.Len(t, out.Output.Message.Content, 1)
	assert.Equal(t, "The model response violates our policy.", aws.StringValue(out.Output.Message.Content[0].Text))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, aws.StringValue(out.StopReason))
}

func TestRouter_Converse_GuardrailOutputAnonymize(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-output-anonymize"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, _ := countingVLLMServer(t, "contact jane@example.com for support")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	out, err := rt.Converse(ctx, grCallerAccount, modelID, guardedConverseInput("hello", createOut.GuardrailId, false))
	require.NoError(t, err)

	require.NotNil(t, out.Output.Message)
	require.Len(t, out.Output.Message.Content, 1)
	assert.Equal(t, "contact {EMAIL} for support", aws.StringValue(out.Output.Message.Content[0].Text))
	// Redaction alone (no block) leaves the backend's own stop reason intact.
	assert.Equal(t, bedrockruntime.StopReasonEndTurn, aws.StringValue(out.StopReason))
}

func TestRouter_Converse_NoGuardrailConfig_Regression(t *testing.T) {
	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, hits := countingVLLMServer(t, "hi there")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	out, err := rt.Converse(context.Background(), "000000000001", modelID, converseInput())
	require.NoError(t, err)

	assert.Equal(t, int32(1), hits.Load())
	require.NotNil(t, out.Output.Message)
	assert.Equal(t, "hi there", aws.StringValue(out.Output.Message.Content[0].Text))
	assert.Nil(t, out.Trace)
}

func TestRouter_Converse_UnknownOrForeignGuardrailReturnsResourceNotFound(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, hits := countingVLLMServer(t, "hi")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	_, err := rt.Converse(ctx, grCallerAccount, modelID, guardedConverseInput("hello", aws.String("does-not-exist"), false))
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-foreign"))
	require.NoError(t, err)

	_, err = rt.Converse(ctx, grOtherCaller, modelID, guardedConverseInput("hello", createOut.GuardrailId, false))
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))

	assert.Equal(t, int32(0), hits.Load(), "an unresolvable guardrail must fail closed before the backend is ever reached")
}

func TestRouter_Converse_TraceEnabledSurfacesAssessment(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-trace-on"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, _ := countingVLLMServer(t, "contact jane@example.com for support")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	out, err := rt.Converse(ctx, grCallerAccount, modelID, guardedConverseInput("hello", createOut.GuardrailId, true))
	require.NoError(t, err)

	require.NotNil(t, out.Trace)
	require.NotNil(t, out.Trace.Guardrail)
	assert.NotEmpty(t, out.Trace.Guardrail.InputAssessment)
	assert.NotEmpty(t, out.Trace.Guardrail.OutputAssessments)
}

func TestRouter_Converse_TraceDisabledOmitsAssessment(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-trace-off"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, _ := countingVLLMServer(t, "contact jane@example.com for support")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	out, err := rt.Converse(ctx, grCallerAccount, modelID, guardedConverseInput("hello", createOut.GuardrailId, false))
	require.NoError(t, err)

	assert.Nil(t, out.Trace)
}

// guardrailStreamConverseInput builds a ConverseStreamInput carrying one user
// message and a GuardrailStreamConfiguration addressing guardrailID's DRAFT.
func guardrailStreamConverseInput(text string, guardrailID *string, trace bool) *bedrockruntime.ConverseStreamInput {
	cfg := &bedrockruntime.GuardrailStreamConfiguration{
		GuardrailIdentifier: guardrailID,
		GuardrailVersion:    aws.String(guardrailDraftVersion),
	}
	if trace {
		cfg.Trace = aws.String(bedrockruntime.GuardrailTraceEnabled)
	}
	return &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String(text)}}},
		},
		GuardrailConfig: cfg,
	}
}

type decodedDelta struct {
	Delta struct {
		Text string `json:"text"`
	} `json:"delta"`
}

type decodedMessageStop struct {
	StopReason string `json:"stopReason"`
}

func TestConverseStream_GuardrailInputBlock_SkipsBackend(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-input-block"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer ts.Close()

	rec := httptest.NewRecorder()
	body, err := json.Marshal(guardrailStreamConverseInput("this has a badword in it", createOut.GuardrailId, false))
	require.NoError(t, err)

	err = ConverseStream(ctx, rec, grCallerAccount, modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(0), hits.Load(), "the backend stream must never be started on an INPUT block")

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 5)
	assert.Equal(t, []string{"messageStart", "contentBlockDelta", "contentBlockStop", "messageStop", "metadata"},
		[]string{frames[0].Type, frames[1].Type, frames[2].Type, frames[3].Type, frames[4].Type})

	var delta decodedDelta
	require.NoError(t, json.Unmarshal(frames[1].Payload, &delta))
	assert.Equal(t, "Your input violates our policy.", delta.Delta.Text)

	var stop decodedMessageStop
	require.NoError(t, json.Unmarshal(frames[3].Payload, &stop))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, stop.StopReason)
}

func TestConverseStream_UnknownOrForeignGuardrailReturnsResourceNotFoundPreHeader(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	modelID := "meta.llama3-2-1b-instruct-v1:0"

	rec := httptest.NewRecorder()
	body, err := json.Marshal(guardrailStreamConverseInput("hello", aws.String("does-not-exist"), false))
	require.NoError(t, err)

	err = ConverseStream(ctx, rec, grCallerAccount, modelID, body, nil, nil, nil, grantAll{}, nil, store, nil)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
	// A pre-stream failure must not have written anything.
	assert.Equal(t, 0, rec.Body.Len())
}

func TestConverseStream_NoGuardrailConfig_Regression(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	rec := httptest.NewRecorder()
	body, err := json.Marshal(converseStreamInput())
	require.NoError(t, err)

	err = ConverseStream(context.Background(), rec, "000000000001", modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)
	require.NoError(t, err)

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 6)
	assert.Equal(t, "contentBlockDelta", frames[1].Type)
	var delta decodedDelta
	require.NoError(t, json.Unmarshal(frames[1].Payload, &delta))
	assert.Equal(t, "Hello", delta.Delta.Text)
}

// guardrailStreamFixtureEvents builds the taxonomy a backend reframer emits
// for a single-block text completion, for driving guardrailStreamSource
// directly without a real HTTP SSE fixture.
func guardrailStreamFixtureEvents(chunks ...string) []ConverseStreamEvent {
	events := []ConverseStreamEvent{
		{Kind: converseStreamEventMessageStart, MessageStart: &bedrockruntime.MessageStartEvent{Role: aws.String(bedrockruntime.ConversationRoleAssistant)}},
	}
	for _, c := range chunks {
		events = append(events, ConverseStreamEvent{
			Kind: converseStreamEventContentBlockDelta,
			ContentBlockDelta: &bedrockruntime.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int64(0),
				Delta:             &bedrockruntime.ContentBlockDelta{Text: aws.String(c)},
			},
		})
	}
	events = append(events,
		ConverseStreamEvent{Kind: converseStreamEventContentBlockStop, ContentBlockStop: &bedrockruntime.ContentBlockStopEvent{ContentBlockIndex: aws.Int64(0)}},
		ConverseStreamEvent{Kind: converseStreamEventMessageStop, MessageStop: &bedrockruntime.MessageStopEvent{StopReason: aws.String(bedrockruntime.StopReasonEndTurn)}},
		ConverseStreamEvent{Kind: converseStreamEventMetadata, Metadata: &bedrockruntime.ConverseStreamMetadataEvent{
			Usage:   &bedrockruntime.TokenUsage{InputTokens: aws.Int64(1), OutputTokens: aws.Int64(2), TotalTokens: aws.Int64(3)},
			Metrics: &bedrockruntime.ConverseStreamMetrics{LatencyMs: aws.Int64(1)},
		}},
	)
	return events
}

func TestGuardrailStreamSource_OutputBlock_BuffersAndReplacesText(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-output-block"))
	require.NoError(t, err)

	inner := &fakeConverseStreamSource{events: guardrailStreamFixtureEvents("this has a ", "badword in it")}
	src := newGuardrailStreamSource(inner, store, nil, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, false, nil)

	events := drainConverseStream(t, src)
	// The two raw deltas collapse into exactly one guarded delta.
	kinds := make([]converseStreamEventKind, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	assert.Equal(t, []converseStreamEventKind{
		converseStreamEventMessageStart,
		converseStreamEventContentBlockDelta,
		converseStreamEventContentBlockStop,
		converseStreamEventMessageStop,
		converseStreamEventMetadata,
	}, kinds)

	deltaEvent := events[1]
	assert.Equal(t, "The model response violates our policy.", aws.StringValue(deltaEvent.ContentBlockDelta.Delta.Text))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, aws.StringValue(events[3].MessageStop.StopReason))
}

func TestGuardrailStreamSource_OutputAnonymize_RedactsAccumulatedText(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-output-anonymize"))
	require.NoError(t, err)

	inner := &fakeConverseStreamSource{events: guardrailStreamFixtureEvents("contact jane@", "example.com for support")}
	src := newGuardrailStreamSource(inner, store, nil, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, false, nil)

	events := drainConverseStream(t, src)
	require.Len(t, events, 5)
	assert.Equal(t, "contact {EMAIL} for support", aws.StringValue(events[1].ContentBlockDelta.Delta.Text))
	// Redaction alone leaves the model's own stop reason intact.
	assert.Equal(t, bedrockruntime.StopReasonEndTurn, aws.StringValue(events[3].MessageStop.StopReason))
}

func TestGuardrailStreamSource_TraceEnabledSurfacesAssessment(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-trace-on"))
	require.NoError(t, err)

	inputAssessments := []*bedrockruntime.GuardrailAssessment{{}}
	inner := &fakeConverseStreamSource{events: guardrailStreamFixtureEvents("contact jane@example.com for support")}
	src := newGuardrailStreamSource(inner, store, nil, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, true, inputAssessments)

	events := drainConverseStream(t, src)
	metadata := events[len(events)-1]
	require.Equal(t, converseStreamEventMetadata, metadata.Kind)
	require.NotNil(t, metadata.Metadata.Trace)
	require.NotNil(t, metadata.Metadata.Trace.Guardrail)
	assert.NotEmpty(t, metadata.Metadata.Trace.Guardrail.InputAssessment)
	assert.NotEmpty(t, metadata.Metadata.Trace.Guardrail.OutputAssessments)
}

// countingLlamaCompletionsServer answers every request with a fixed
// non-streaming Llama /v1/completions response carrying text, and a counter
// of how many requests it actually received.
func countingLlamaCompletionsServer(t *testing.T, text string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "` + text + `", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

func TestInvokeRouter_GuardrailInputBlock_Llama_SkipsBackend(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-input-block-llama"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, hits := countingLlamaCompletionsServer(t, "hi")
	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	respBody, contentType, err := rt.InvokeModel(ctx, grCallerAccount, modelID, []byte(`{"prompt":"this has a badword in it"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.NoError(t, err)
	assert.Equal(t, int32(0), hits.Load(), "the backend must never be called on an INPUT block")
	assert.Equal(t, "application/json", contentType)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "Your input violates our policy.", out.Generation)
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, out.StopReason)
}

// TestInvokeRouter_GuardrailInputBlock_Anthropic_SkipsBackend proves the
// INPUT check runs -- and short-circuits -- before the Anthropic adapter's
// InvokeModel ever opens a connection: no local server is needed at all,
// since a blocked request must never reach the network.
func TestInvokeRouter_GuardrailInputBlock_Anthropic_SkipsBackend(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-input-block-anthropic"))
	require.NoError(t, err)

	modelID := "anthropic.claude-3-5-sonnet-20240620-v1:0"
	withProviderCatalogEntry(t, modelID)
	rt := NewInvokeRouter(stubCredentialResolver{key: "sk-test", ok: true}, nil, nil, grantAll{}, nil, store, nil)

	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"this has a badword in it"}]}]}`)
	respBody, contentType, err := rt.InvokeModel(ctx, grCallerAccount, modelID, body, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.NoError(t, err)
	assert.Equal(t, "application/json", contentType)

	texts, ok := extractAnthropicCompletionTexts(respBody)
	require.True(t, ok)
	assert.Equal(t, []string{"Your input violates our policy."}, texts)
}

func TestInvokeRouter_GuardrailOutputBlock_Llama(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-output-block-llama"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, _ := countingLlamaCompletionsServer(t, "this has a badword in it")
	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	respBody, _, err := rt.InvokeModel(ctx, grCallerAccount, modelID, []byte(`{"prompt":"hello"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.NoError(t, err)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "The model response violates our policy.", out.Generation)
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, out.StopReason)
}

func TestInvokeRouter_GuardrailOutputAnonymize_Llama(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-output-anonymize-llama"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, _ := countingLlamaCompletionsServer(t, "contact jane@example.com for support")
	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	respBody, _, err := rt.InvokeModel(ctx, grCallerAccount, modelID, []byte(`{"prompt":"hello"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.NoError(t, err)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "contact {EMAIL} for support", out.Generation)
	// Redaction alone (no block) leaves the backend's own stop reason intact.
	assert.Equal(t, "stop", out.StopReason)
}

func TestInvokeRouter_NoGuardrailHeader_Regression(t *testing.T) {
	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, hits := countingLlamaCompletionsServer(t, "hi there")
	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil, nil)

	respBody, _, err := rt.InvokeModel(context.Background(), "000000000001", modelID, []byte(`{"prompt":"hello"}`), "", "")
	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load())

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Equal(t, "hi there", out.Generation)
}

func TestInvokeRouter_UnknownOrForeignGuardrailReturnsResourceNotFound(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, hits := countingLlamaCompletionsServer(t, "hi")
	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	_, _, err := rt.InvokeModel(ctx, grCallerAccount, modelID, []byte(`{"prompt":"hello"}`), "does-not-exist", guardrailDraftVersion)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-foreign"))
	require.NoError(t, err)

	_, _, err = rt.InvokeModel(ctx, grOtherCaller, modelID, []byte(`{"prompt":"hello"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))

	assert.Equal(t, int32(0), hits.Load(), "an unresolvable guardrail must fail closed before the backend is ever reached")
}

// llamaBadwordStreamFixture is a canned OpenAI /v1/completions streaming SSE
// body whose accumulated text trips the "badword" blocklist entry.
const llamaBadwordStreamFixture = `data: {"choices":[{"text":"this has a ","finish_reason":null}]}

data: {"choices":[{"text":"badword in it","finish_reason":null}]}

data: {"choices":[{"text":"","finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2}}

data: [DONE]

`

// llamaAnonymizeStreamFixture is llamaBadwordStreamFixture's sibling whose
// accumulated text trips the email PII ANONYMIZE entry instead.
const llamaAnonymizeStreamFixture = `data: {"choices":[{"text":"contact jane@","finish_reason":null}]}

data: {"choices":[{"text":"example.com for support","finish_reason":null}]}

data: {"choices":[{"text":"","finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2}}

data: [DONE]

`

func TestInvokeStreamRouter_GuardrailInputBlock_SkipsBackend(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-stream-input-block"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	defer ts.Close()

	rt := NewInvokeStreamRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, nil, store, nil)

	src, err := rt.InvokeModelWithResponseStream(ctx, grCallerAccount, modelID, []byte(`{"prompt":"this has a badword in it"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.NoError(t, err)
	chunks := drainInvokeStream(t, src)
	assert.Equal(t, int32(0), hits.Load(), "the backend stream must never be started on an INPUT block")
	require.Len(t, chunks, 2)

	var delta llamaInvokeStreamChunk
	require.NoError(t, json.Unmarshal(chunks[0], &delta))
	assert.Equal(t, "Your input violates our policy.", delta.Generation)

	var final llamaInvokeStreamFinalChunk
	require.NoError(t, json.Unmarshal(chunks[1], &final))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, final.StopReason)
}

func TestInvokeStreamRouter_GuardrailOutputBlock_Buffered(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-stream-output-block"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaBadwordStreamFixture))
	}))
	defer ts.Close()

	rt := NewInvokeStreamRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, nil, store, nil)

	src, err := rt.InvokeModelWithResponseStream(ctx, grCallerAccount, modelID, []byte(`{"prompt":"hello"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.NoError(t, err)
	chunks := drainInvokeStream(t, src)
	require.Len(t, chunks, 2, "the raw deltas must collapse into exactly one guarded delta + final chunk")

	var delta llamaInvokeStreamChunk
	require.NoError(t, json.Unmarshal(chunks[0], &delta))
	assert.Equal(t, "The model response violates our policy.", delta.Generation)

	var final llamaInvokeStreamFinalChunk
	require.NoError(t, json.Unmarshal(chunks[1], &final))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, final.StopReason)
}

func TestInvokeStreamRouter_GuardrailOutputAnonymize_Buffered(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-stream-output-anonymize"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaAnonymizeStreamFixture))
	}))
	defer ts.Close()

	rt := NewInvokeStreamRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, nil, store, nil)

	src, err := rt.InvokeModelWithResponseStream(ctx, grCallerAccount, modelID, []byte(`{"prompt":"hello"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.NoError(t, err)
	chunks := drainInvokeStream(t, src)
	require.Len(t, chunks, 2)

	var delta llamaInvokeStreamChunk
	require.NoError(t, json.Unmarshal(chunks[0], &delta))
	assert.Equal(t, "contact {EMAIL} for support", delta.Generation)

	var final llamaInvokeStreamFinalChunk
	require.NoError(t, json.Unmarshal(chunks[1], &final))
	// Redaction alone leaves the model's own stop reason intact.
	assert.Equal(t, "stop", final.StopReason)
}

func TestInvokeStreamRouter_NoGuardrailHeader_Regression(t *testing.T) {
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
	chunks := drainInvokeStream(t, src)
	require.Len(t, chunks, 3)

	var delta llamaInvokeStreamChunk
	require.NoError(t, json.Unmarshal(chunks[0], &delta))
	assert.Equal(t, "Hello", delta.Generation)
}

// TestExtractAnthropicPromptTexts_SystemAndUserOnly covers the pieces
// extractInvokePromptTexts reads for the Anthropic family: the system prompt
// plus every user message's text blocks, skipping assistant turns.
func TestExtractAnthropicPromptTexts_SystemAndUserOnly(t *testing.T) {
	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"system":"be nice","messages":[{"role":"user","content":[{"type":"text","text":"hello there"}]},{"role":"assistant","content":[{"type":"text","text":"ignored"}]}]}`)
	texts, ok := extractAnthropicPromptTexts(body)
	require.True(t, ok)
	assert.Equal(t, []string{"be nice", "hello there"}, texts)
}

// TestAnthropicCompletionGuardrailHelpers_PreserveUnrelatedFields drives the
// OUTPUT-side Anthropic helpers directly (no live network double exists for
// the Anthropic family at Router level, mirroring the rest of this package's
// InvokeAdapter-level Anthropic coverage): redaction and a wholesale block
// must both leave id/model/usage untouched.
func TestAnthropicCompletionGuardrailHelpers_PreserveUnrelatedFields(t *testing.T) {
	respBody := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20240620","content":[{"type":"text","text":"contact jane@example.com for support"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)

	texts, ok := extractAnthropicCompletionTexts(respBody)
	require.True(t, ok)
	assert.Equal(t, []string{"contact jane@example.com for support"}, texts)

	redacted, err := setAnthropicCompletionTexts(respBody, []string{"contact {EMAIL} for support"})
	require.NoError(t, err)
	texts, ok = extractAnthropicCompletionTexts(redacted)
	require.True(t, ok)
	assert.Equal(t, []string{"contact {EMAIL} for support"}, texts)
	assert.Contains(t, string(redacted), `"msg_1"`)
	assert.Contains(t, string(redacted), `"input_tokens":1`)

	blocked, err := replaceAnthropicCompletion(respBody, "The model response violates our policy.")
	require.NoError(t, err)
	texts, ok = extractAnthropicCompletionTexts(blocked)
	require.True(t, ok)
	assert.Equal(t, []string{"The model response violates our policy."}, texts)
	assert.Contains(t, string(blocked), bedrockruntime.StopReasonGuardrailIntervened)
	assert.Contains(t, string(blocked), `"msg_1"`)

	inputBlocked, err := buildAnthropicBlockedResponse("anthropic.claude-3-5-sonnet-20240620-v1:0", "Your input violates our policy.")
	require.NoError(t, err)
	texts, ok = extractAnthropicCompletionTexts(inputBlocked)
	require.True(t, ok)
	assert.Equal(t, []string{"Your input violates our policy."}, texts)
}

func TestGuardrailStreamSource_TraceDisabledOmitsAssessment(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-trace-off"))
	require.NoError(t, err)

	inner := &fakeConverseStreamSource{events: guardrailStreamFixtureEvents("contact jane@example.com for support")}
	src := newGuardrailStreamSource(inner, store, nil, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, false, nil)

	events := drainConverseStream(t, src)
	metadata := events[len(events)-1]
	require.Equal(t, converseStreamEventMetadata, metadata.Kind)
	assert.Nil(t, metadata.Metadata.Trace)
}

// TestAnthropicInvokeAdapter_UndecodableOutputBlocksWithMessaging proves the
// OUTPUT hook's fail-closed building blocks for the Anthropic family end to
// end at the adapter boundary: anthropicInvokeAdapter.InvokeModel forwards
// the upstream body verbatim (unlike Llama's, which always re-marshals a
// validated Go struct before returning), so a malformed upstream response
// really does reach extractAnthropicCompletionTexts undecodable in
// production -- exactly the extraction failure invoke.go's OUTPUT hook now
// fails closed on, using invokeGuardrailBlockedResponse to build the guarded
// reply.
func TestAnthropicInvokeAdapter_UndecodableOutputBlocksWithMessaging(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer ts.Close()

	a := &anthropicInvokeAdapter{httpClient: ts.Client(), baseURL: ts.URL}
	reqBody := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	respBody, _, err := a.InvokeModel(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", reqBody, "sk-test")
	require.NoError(t, err, "the adapter forwards a non-2xx-but-still-200 malformed body verbatim rather than erroring itself")
	assert.Equal(t, "not json", string(respBody))

	_, ok := extractAnthropicCompletionTexts(respBody)
	require.False(t, ok, "a non-JSON upstream body must fail to decode as the expected shape")

	blocked, err := invokeGuardrailBlockedResponse(providerPrefix+vendorAnthropic, "anthropic.claude-3-5-sonnet-20240620-v1:0", "The model response violates our policy.")
	require.NoError(t, err)

	texts, ok := extractAnthropicCompletionTexts(blocked)
	require.True(t, ok)
	assert.Equal(t, []string{"The model response violates our policy."}, texts)
	assert.NotContains(t, string(blocked), "not json")
}

// TestLlamaCompletionGuardrailHelpers_UndecodableBodyBlocksWithMessaging
// exercises the same extractLlamaCompletionTexts/invokeGuardrailBlockedResponse
// pair the OUTPUT hook's fail-closed branch calls for the Llama family.
// Unlike Anthropic, llamaInvokeAdapter.InvokeModel always re-marshals a
// validated llamaInvokeResponse before returning, so an undecodable body can
// never actually reach the hook through the real self-host adapter chain --
// this proves the building blocks fail closed regardless.
func TestLlamaCompletionGuardrailHelpers_UndecodableBodyBlocksWithMessaging(t *testing.T) {
	badBody := []byte(`{"generation": 12345}`)

	_, ok := extractLlamaCompletionTexts(badBody)
	require.False(t, ok, "a numeric generation field must fail to decode as the expected string shape")

	blocked, err := invokeGuardrailBlockedResponse(tierSelfHost, "meta.llama3-2-1b-instruct-v1:0", "The model response violates our policy.")
	require.NoError(t, err)

	var out llamaInvokeResponse
	require.NoError(t, json.Unmarshal(blocked, &out))
	assert.Equal(t, "The model response violates our policy.", out.Generation)
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, out.StopReason)
	assert.NotContains(t, string(blocked), "12345")
}

// TestInvokeRouter_GuardrailOutputPath_DeletedMidRequest_FailsClosed proves
// loadGuardrailView's reload on the OUTPUT path fails closed: the guardrail
// is deleted from inside the backend handler, after the INPUT check already
// passed (the backend is only ever reached once INPUT allowed the request
// through) but before the OUTPUT check runs its own reload.
func TestInvokeRouter_GuardrailOutputPath_DeletedMidRequest_FailsClosed(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-output-guardrail-deleted-mid-request"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, derr := DeleteGuardrail(context.Background(), grCallerAccount, store, &bedrock.DeleteGuardrailInput{GuardrailIdentifier: createOut.GuardrailId})
		assert.NoError(t, derr)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"text": "hi", "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer ts.Close()

	rt := NewInvokeRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store, nil)

	respBody, _, err := rt.InvokeModel(ctx, grCallerAccount, modelID, []byte(`{"prompt":"hello"}`), aws.StringValue(createOut.GuardrailId), guardrailDraftVersion)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
	assert.Nil(t, respBody, "a fail-closed OUTPUT-path guardrail-load failure must not leak a body")
}

// TestGuardrailInvokeStreamSource_OutputBlock_UndecodableChunk_Llama drives
// guardrailInvokeStreamSource.Next directly with a scripted chunk sequence
// (independent of any real backend) whose second chunk fails to decode --
// the buffered/SYNC invoke-stream analog of TestInvokeRouter_
// GuardrailOutputBlock_UndecodableBody_Anthropic.
func TestGuardrailInvokeStreamSource_OutputBlock_UndecodableChunk_Llama(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-stream-output-undecodable-llama"))
	require.NoError(t, err)

	inner := &fakeInvokeStreamSource{chunks: [][]byte{
		[]byte(`{"generation":"partial safe text "}`),
		[]byte(`{"generation": 123}`),
	}}
	g := newGuardrailInvokeStreamSource(inner, store, nil, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, tierSelfHost)

	chunks := drainInvokeStream(t, g)
	require.Len(t, chunks, 2, "the guarded blocked sequence replaces the raw buffer wholesale")

	var delta llamaInvokeStreamChunk
	require.NoError(t, json.Unmarshal(chunks[0], &delta))
	assert.Equal(t, "The model response violates our policy.", delta.Generation)

	var final llamaInvokeStreamFinalChunk
	require.NoError(t, json.Unmarshal(chunks[1], &final))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, final.StopReason)

	for _, c := range chunks {
		assert.NotContains(t, string(c), "partial safe text")
	}
}

// TestGuardrailInvokeStreamSource_OutputBlock_UndecodableChunk_Anthropic is
// TestGuardrailInvokeStreamSource_OutputBlock_UndecodableChunk_Llama's
// Anthropic-family sibling: a content_block_delta chunk whose text field is
// the wrong JSON type fails to decode outright, distinct from a
// structurally valid control chunk (content_block_start/stop) carrying no
// text at all.
func TestGuardrailInvokeStreamSource_OutputBlock_UndecodableChunk_Anthropic(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-stream-output-undecodable-anthropic"))
	require.NoError(t, err)

	backend := providerPrefix + vendorAnthropic
	inner := &fakeInvokeStreamSource{chunks: [][]byte{
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial safe text "}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":123}}`),
	}}
	g := newGuardrailInvokeStreamSource(inner, store, nil, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, backend)

	chunks := drainInvokeStream(t, g)
	require.Len(t, chunks, 5, "anthropicGuardedStreamChunks' minimal event sequence")

	var contentDelta struct {
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	require.NoError(t, json.Unmarshal(chunks[1], &contentDelta))
	assert.Equal(t, "The model response violates our policy.", contentDelta.Delta.Text)

	var msgDelta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	require.NoError(t, json.Unmarshal(chunks[3], &msgDelta))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, msgDelta.Delta.StopReason)

	for _, c := range chunks {
		assert.NotContains(t, string(c), "partial safe text")
	}
}

// TestGuardrailInvokeStreamSource_EmptyCompletion_PassesThroughUnblocked_Regression
// proves a genuinely empty completion (every chunk decodes fine, none carry
// assistant text) is forwarded unchanged rather than blocked -- the decode-
// failure signal must never fire on a benign control/empty chunk.
func TestGuardrailInvokeStreamSource_EmptyCompletion_PassesThroughUnblocked_Regression(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("invoke-stream-output-empty-regression"))
	require.NoError(t, err)

	rawChunks := [][]byte{
		[]byte(`{"generation":""}`),
		[]byte(`{"generation":"","prompt_token_count":1,"generation_token_count":0,"stop_reason":"stop"}`),
	}
	inner := &fakeInvokeStreamSource{chunks: append([][]byte{}, rawChunks...)}
	g := newGuardrailInvokeStreamSource(inner, store, nil, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, tierSelfHost)

	chunks := drainInvokeStream(t, g)
	require.Len(t, chunks, len(rawChunks))
	for i, c := range chunks {
		assert.JSONEq(t, string(rawChunks[i]), string(c))
	}
}
