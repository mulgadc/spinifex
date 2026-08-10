package gateway_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
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
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store)

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
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store)

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
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store)

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
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil)

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
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store)

	_, err := rt.Converse(ctx, grCallerAccount, modelID, guardedConverseInput("hello", aws.String("does-not-exist"), false))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-foreign"))
	require.NoError(t, err)

	_, err = rt.Converse(ctx, grOtherCaller, modelID, guardedConverseInput("hello", createOut.GuardrailId, false))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	assert.Equal(t, int32(0), hits.Load(), "an unresolvable guardrail must fail closed before the backend is ever reached")
}

func TestRouter_Converse_TraceEnabledSurfacesAssessment(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("converse-trace-on"))
	require.NoError(t, err)

	modelID := "meta.llama3-2-1b-instruct-v1:0"
	ts, _ := countingVLLMServer(t, "contact jane@example.com for support")
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store)

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
	rt := NewRouter(nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store)

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

	err = ConverseStream(ctx, rec, grCallerAccount, modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, store)
	require.NoError(t, err)
	assert.Equal(t, int32(0), hits.Load(), "the backend stream must never be started on an INPUT block")

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 6)
	assert.Equal(t, []string{"messageStart", "contentBlockStart", "contentBlockDelta", "contentBlockStop", "messageStop", "metadata"},
		[]string{frames[0].Type, frames[1].Type, frames[2].Type, frames[3].Type, frames[4].Type, frames[5].Type})

	var delta decodedDelta
	require.NoError(t, json.Unmarshal(frames[2].Payload, &delta))
	assert.Equal(t, "Your input violates our policy.", delta.Delta.Text)

	var stop decodedMessageStop
	require.NoError(t, json.Unmarshal(frames[4].Payload, &stop))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, stop.StopReason)
}

func TestConverseStream_UnknownOrForeignGuardrailReturnsResourceNotFoundPreHeader(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	modelID := "meta.llama3-2-1b-instruct-v1:0"

	rec := httptest.NewRecorder()
	body, err := json.Marshal(guardrailStreamConverseInput("hello", aws.String("does-not-exist"), false))
	require.NoError(t, err)

	err = ConverseStream(ctx, rec, grCallerAccount, modelID, body, nil, nil, nil, grantAll{}, nil, store)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
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

	err = ConverseStream(context.Background(), rec, "000000000001", modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), nil, grantAll{}, nil, nil)
	require.NoError(t, err)

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 7)
	assert.Equal(t, "contentBlockDelta", frames[2].Type)
	var delta decodedDelta
	require.NoError(t, json.Unmarshal(frames[2].Payload, &delta))
	assert.Equal(t, "Hello", delta.Delta.Text)
}

// guardrailStreamFixtureEvents builds the taxonomy a backend reframer emits
// for a single-block text completion, for driving guardrailStreamSource
// directly without a real HTTP SSE fixture.
func guardrailStreamFixtureEvents(chunks ...string) []ConverseStreamEvent {
	events := []ConverseStreamEvent{
		{Kind: converseStreamEventMessageStart, MessageStart: &bedrockruntime.MessageStartEvent{Role: aws.String(bedrockruntime.ConversationRoleAssistant)}},
		{Kind: converseStreamEventContentBlockStart, ContentBlockStart: &bedrockruntime.ContentBlockStartEvent{ContentBlockIndex: aws.Int64(0), Start: &bedrockruntime.ContentBlockStart{}}},
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
	src := newGuardrailStreamSource(inner, store, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, false, nil)

	events := drainConverseStream(t, src)
	// The two raw deltas collapse into exactly one guarded delta.
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

	deltaEvent := events[2]
	assert.Equal(t, "The model response violates our policy.", aws.StringValue(deltaEvent.ContentBlockDelta.Delta.Text))
	assert.Equal(t, bedrockruntime.StopReasonGuardrailIntervened, aws.StringValue(events[4].MessageStop.StopReason))
}

func TestGuardrailStreamSource_OutputAnonymize_RedactsAccumulatedText(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-output-anonymize"))
	require.NoError(t, err)

	inner := &fakeConverseStreamSource{events: guardrailStreamFixtureEvents("contact jane@", "example.com for support")}
	src := newGuardrailStreamSource(inner, store, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, false, nil)

	events := drainConverseStream(t, src)
	require.Len(t, events, 6)
	assert.Equal(t, "contact {EMAIL} for support", aws.StringValue(events[2].ContentBlockDelta.Delta.Text))
	// Redaction alone leaves the model's own stop reason intact.
	assert.Equal(t, bedrockruntime.StopReasonEndTurn, aws.StringValue(events[4].MessageStop.StopReason))
}

func TestGuardrailStreamSource_TraceEnabledSurfacesAssessment(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-trace-on"))
	require.NoError(t, err)

	inputAssessments := []*bedrockruntime.GuardrailAssessment{{}}
	inner := &fakeConverseStreamSource{events: guardrailStreamFixtureEvents("contact jane@example.com for support")}
	src := newGuardrailStreamSource(inner, store, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, true, inputAssessments)

	events := drainConverseStream(t, src)
	metadata := events[len(events)-1]
	require.Equal(t, converseStreamEventMetadata, metadata.Kind)
	require.NotNil(t, metadata.Metadata.Trace)
	require.NotNil(t, metadata.Metadata.Trace.Guardrail)
	assert.NotEmpty(t, metadata.Metadata.Trace.Guardrail.InputAssessment)
	assert.NotEmpty(t, metadata.Metadata.Trace.Guardrail.OutputAssessments)
}

func TestGuardrailStreamSource_TraceDisabledOmitsAssessment(t *testing.T) {
	store := newGuardrailTestStore(t)
	ctx := context.Background()
	createOut, err := CreateGuardrail(ctx, grCallerAccount, store, createGuardrailInput("stream-trace-off"))
	require.NoError(t, err)

	inner := &fakeConverseStreamSource{events: guardrailStreamFixtureEvents("contact jane@example.com for support")}
	src := newGuardrailStreamSource(inner, store, grCallerAccount, aws.StringValue(createOut.GuardrailId), guardrailDraftVersion, false, nil)

	events := drainConverseStream(t, src)
	metadata := events[len(events)-1]
	require.Equal(t, converseStreamEventMetadata, metadata.Kind)
	assert.Nil(t, metadata.Metadata.Trace)
}
