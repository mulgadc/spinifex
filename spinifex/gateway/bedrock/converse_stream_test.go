package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	esv2 "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConverseStreamSource drives pumpConverseStream directly with a scripted
// event/error sequence, independent of any real backend.
type fakeConverseStreamSource struct {
	events []ConverseStreamEvent
	err    error
	closed bool
}

func (f *fakeConverseStreamSource) Next(_ context.Context) (ConverseStreamEvent, bool, error) {
	if len(f.events) > 0 {
		ev := f.events[0]
		f.events = f.events[1:]
		return ev, true, nil
	}
	if f.err != nil {
		return ConverseStreamEvent{}, false, f.err
	}
	return ConverseStreamEvent{}, false, nil
}

func (f *fakeConverseStreamSource) Close() error {
	f.closed = true
	return nil
}

// decodeAllFrames decodes every event-stream message in raw via the v2
// package Decoder, returning each frame's ":message-type"/":event-type" (or
// ":exception-type") header and payload.
type decodedFrame struct {
	MessageType string
	Type        string
	Payload     []byte
}

func decodeAllFrames(t *testing.T, raw []byte) []decodedFrame {
	t.Helper()
	dec := esv2.NewDecoder()
	r := bytes.NewReader(raw)
	var out []decodedFrame
	for r.Len() > 0 {
		msg, err := dec.Decode(r, nil)
		require.NoError(t, err)
		mt := headerString(t, msg, ":message-type")
		typeHeader := ":event-type"
		if mt == "exception" {
			typeHeader = ":exception-type"
		}
		out = append(out, decodedFrame{MessageType: mt, Type: headerString(t, msg, typeHeader), Payload: msg.Payload})
	}
	return out
}

func TestPumpConverseStream_MidStreamFaultWritesExceptionFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	src := &fakeConverseStreamSource{
		events: []ConverseStreamEvent{
			{Kind: converseStreamEventMessageStart, MessageStart: &bedrockruntime.MessageStartEvent{Role: aws.String("assistant")}},
		},
		err: newStreamFault(errors.New("upstream connection reset")),
	}

	pumpConverseStream(context.Background(), fw, src, "test-model")

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 2)
	assert.Equal(t, "event", frames[0].MessageType)
	assert.Equal(t, "messageStart", frames[0].Type)
	assert.Equal(t, "exception", frames[1].MessageType)
	assert.Equal(t, excModelStreamErrorException, frames[1].Type)
	assert.JSONEq(t, `{"message":"upstream connection reset"}`, string(frames[1].Payload))

	// The status must still be 200: post-header, a fault cannot change it.
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestPumpConverseStream_NonFaultErrorUsesInternalServerException(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	src := &fakeConverseStreamSource{err: errors.New("plain internal error")}
	pumpConverseStream(context.Background(), fw, src, "test-model")

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 1)
	assert.Equal(t, excInternalServerException, frames[0].Type)
}

func TestPumpConverseStream_CleanEOFWritesNoExceptionFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	src := &fakeConverseStreamSource{
		events: []ConverseStreamEvent{
			{Kind: converseStreamEventMessageStart, MessageStart: &bedrockruntime.MessageStartEvent{Role: aws.String("assistant")}},
			{Kind: converseStreamEventMessageStop, MessageStop: &bedrockruntime.MessageStopEvent{StopReason: aws.String(bedrockruntime.StopReasonEndTurn)}},
		},
	}
	pumpConverseStream(context.Background(), fw, src, "test-model")

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 2)
	assert.Equal(t, "messageStart", frames[0].Type)
	assert.Equal(t, "messageStop", frames[1].Type)
}

func TestConverseStream_UnknownModelReturnsResourceNotFoundPreHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)

	err := ConverseStream(context.Background(), rec, "000000000001", "does.not-exist-v1:0", body, nil, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	// A pre-stream failure must not have written anything: the gateway's
	// ErrorHandler owns turning this into the JSON error envelope.
	assert.Equal(t, 0, rec.Body.Len())
}

func TestConverseStream_MalformedBodyReturnsValidationException(t *testing.T) {
	rec := httptest.NewRecorder()
	err := ConverseStream(context.Background(), rec, "000000000001", "meta.llama3-70b-instruct-v1:0", []byte("{not-json"), nil, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

func TestConverseStream_SelfHostHappyPath_WritesFramedTaxonomy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	rec := httptest.NewRecorder()
	body, err := json.Marshal(&bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	})
	require.NoError(t, err)

	err = ConverseStream(context.Background(), rec, "000000000001", modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, eventStreamContentType, rec.Header().Get("Content-Type"))

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 7)
	assert.Equal(t, "messageStart", frames[0].Type)
	assert.Equal(t, "contentBlockStart", frames[1].Type)
	assert.Equal(t, "contentBlockDelta", frames[2].Type)
	assert.Equal(t, "contentBlockDelta", frames[3].Type)
	assert.Equal(t, "contentBlockStop", frames[4].Type)
	assert.Equal(t, "messageStop", frames[5].Type)
	assert.Equal(t, "metadata", frames[6].Type)
}

func TestConverseStream_NonFlusherWriter_ReturnsPreHeaderError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	w := &nonFlusherWriter{}
	body, err := json.Marshal(&bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	})
	require.NoError(t, err)

	err = ConverseStream(context.Background(), w, "000000000001", modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.False(t, w.wroteHeader)
}
