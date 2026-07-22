package gateway_bedrock

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/awstesting/unit"
	"github.com/aws/aws-sdk-go/private/protocol/eventstream/eventstreamtest"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the contract test the plan's acceptance criterion calls for:
// feed our production reframers' output through the *real* aws-sdk-go v1
// bedrockruntime client (not just decode the raw bytes ourselves), proving a
// genuine SDK consumer can read what we frame — byte framing, headers, and
// JSON payload shape all have to be right for this to work at all.

// contractSession stands up an httptest server (TLS+h2, matching a real
// event-stream transport) running handler, and returns a session pointed at
// it plus a bedrockruntime client built on that session.
func contractSession(t *testing.T, handler http.HandlerFunc) *bedrockruntime.BedrockRuntime {
	t.Helper()
	sess, cleanup, err := eventstreamtest.SetupEventStreamSession(t, handler, true)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return bedrockruntime.New(sess, &aws.Config{Credentials: unit.Session.Config.Credentials})
}

var converseStreamPathPattern = regexp.MustCompile(`^/model/([^/]+)/converse-stream$`)
var invokeStreamPathPattern = regexp.MustCompile(`^/model/([^/]+)/invoke-with-response-stream$`)

func TestContract_ConverseStream_SelfHost_RealSDKConsumerDecodesFrames(t *testing.T) {
	vllmStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(vllmStreamFixture))
	}))
	defer vllmStub.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	endpoints := NewStaticEndpointResolver(map[string]string{modelID: vllmStub.URL})

	svc := contractSession(t, func(w http.ResponseWriter, r *http.Request) {
		m := converseStreamPathPattern.FindStringSubmatch(r.URL.Path)
		if !assert.NotNil(t, m, "unexpected path %s", r.URL.Path) {
			return
		}
		body, err := readAll(r)
		if !assert.NoError(t, err) {
			return
		}
		err = ConverseStream(r.Context(), w, "000000000001", m[1], body, nil, endpoints, grantAll{})
		assert.NoError(t, err)
	})

	resp, err := svc.ConverseStream(&bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(modelID),
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	})
	require.NoError(t, err)
	defer func() { _ = resp.GetStream().Close() }()

	var kinds []string
	var deltas []string
	for event := range resp.GetStream().Events() {
		switch e := event.(type) {
		case *bedrockruntime.MessageStartEvent:
			kinds = append(kinds, "messageStart")
			assert.Equal(t, bedrockruntime.ConversationRoleAssistant, *e.Role)
		case *bedrockruntime.ContentBlockStartEvent:
			kinds = append(kinds, "contentBlockStart")
		case *bedrockruntime.ContentBlockDeltaEvent:
			kinds = append(kinds, "contentBlockDelta")
			deltas = append(deltas, *e.Delta.Text)
		case *bedrockruntime.ContentBlockStopEvent:
			kinds = append(kinds, "contentBlockStop")
		case *bedrockruntime.MessageStopEvent:
			kinds = append(kinds, "messageStop")
			assert.Equal(t, bedrockruntime.StopReasonEndTurn, *e.StopReason)
		case *bedrockruntime.ConverseStreamMetadataEvent:
			kinds = append(kinds, "metadata")
			assert.Equal(t, int64(8), *e.Usage.InputTokens)
			assert.Equal(t, int64(3), *e.Usage.OutputTokens)
		default:
			t.Fatalf("unexpected event type %T", event)
		}
	}
	require.NoError(t, resp.GetStream().Err())

	assert.Equal(t, []string{
		"messageStart", "contentBlockStart", "contentBlockDelta", "contentBlockDelta",
		"contentBlockStop", "messageStop", "metadata",
	}, kinds)
	assert.Equal(t, []string{"Hello", " world"}, deltas)
}

func TestContract_ConverseStream_Anthropic_RealSDKConsumerDecodesFrames(t *testing.T) {
	anthropicStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicStreamFixture))
	}))
	defer anthropicStub.Close()

	modelID := "anthropic.claude-3-5-sonnet-20240620-v1:0"

	// The top-level ConverseStream entrypoint routes provider-direct traffic
	// through Router, which hardcodes the real Anthropic base URL — there is
	// no injection seam for it (by design: it's a live vendor endpoint, not
	// a pinned self-host one). So this contract test drives the same
	// production pieces (anthropicProvider.ConverseStream + pumpConverseStream
	// + frameWriter) directly against the stub, rather than through
	// ConverseStream(ctx, w, ...): the framing/encoding under test is
	// identical either way, since both operations share pumpConverseStream.
	svc := contractSession(t, func(w http.ResponseWriter, r *http.Request) {
		p := &anthropicProvider{httpClient: anthropicStub.Client(), baseURL: anthropicStub.URL}
		src, err := p.ConverseStream(r.Context(), modelID, &bedrockruntime.ConverseStreamInput{
			Messages: []*bedrockruntime.Message{
				{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}}},
			},
		}, "sk-test")
		if !assert.NoError(t, err) {
			return
		}
		defer func() { _ = src.Close() }()

		fw, err := newFrameWriter(w)
		if !assert.NoError(t, err) {
			return
		}
		pumpConverseStream(r.Context(), fw, src, modelID)
	})

	resp, err := svc.ConverseStream(&bedrockruntime.ConverseStreamInput{ModelId: aws.String(modelID)})
	require.NoError(t, err)
	defer func() { _ = resp.GetStream().Close() }()

	var kinds []string
	var sawToolUse bool
	for event := range resp.GetStream().Events() {
		switch e := event.(type) {
		case *bedrockruntime.MessageStartEvent:
			kinds = append(kinds, "messageStart")
		case *bedrockruntime.ContentBlockStartEvent:
			kinds = append(kinds, "contentBlockStart")
			if e.Start != nil && e.Start.ToolUse != nil {
				sawToolUse = true
				assert.Equal(t, "get_weather", *e.Start.ToolUse.Name)
			}
		case *bedrockruntime.ContentBlockDeltaEvent:
			kinds = append(kinds, "contentBlockDelta")
		case *bedrockruntime.ContentBlockStopEvent:
			kinds = append(kinds, "contentBlockStop")
		case *bedrockruntime.MessageStopEvent:
			kinds = append(kinds, "messageStop")
			assert.Equal(t, bedrockruntime.StopReasonToolUse, *e.StopReason)
		case *bedrockruntime.ConverseStreamMetadataEvent:
			kinds = append(kinds, "metadata")
			assert.Equal(t, int64(12), *e.Usage.InputTokens)
			assert.Equal(t, int64(5), *e.Usage.OutputTokens)
		default:
			t.Fatalf("unexpected event type %T", event)
		}
	}
	require.NoError(t, resp.GetStream().Err())

	assert.Equal(t, []string{
		"messageStart", "contentBlockStart", "contentBlockDelta", "contentBlockDelta", "contentBlockStop",
		"contentBlockStart", "contentBlockDelta", "contentBlockStop", "messageStop", "metadata",
	}, kinds)
	assert.True(t, sawToolUse)
}

func TestContract_InvokeModelWithResponseStream_SelfHost_RealSDKConsumerDecodesFrames(t *testing.T) {
	llamaStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	defer llamaStub.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	endpoints := NewStaticEndpointResolver(map[string]string{modelID: llamaStub.URL})

	svc := contractSession(t, func(w http.ResponseWriter, r *http.Request) {
		m := invokeStreamPathPattern.FindStringSubmatch(r.URL.Path)
		if !assert.NotNil(t, m, "unexpected path %s", r.URL.Path) {
			return
		}
		body, err := readAll(r)
		if !assert.NoError(t, err) {
			return
		}
		err = InvokeModelWithResponseStream(r.Context(), w, "000000000001", m[1], body, nil, endpoints, grantAll{}, r.Header.Get("Content-Type"))
		assert.NoError(t, err)
	})

	resp, err := svc.InvokeModelWithResponseStream(&bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId: aws.String(modelID),
		Body:    []byte(`{"prompt":"hello","max_gen_len":128}`),
	})
	require.NoError(t, err)
	defer func() { _ = resp.GetStream().Close() }()

	var generations []string
	var sawFinal bool
	for event := range resp.GetStream().Events() {
		part, ok := event.(*bedrockruntime.PayloadPart)
		require.True(t, ok, "unexpected event type %T", event)

		var probe struct {
			Generation string `json:"generation"`
			StopReason string `json:"stop_reason"`
		}
		require.NoError(t, json.Unmarshal(part.Bytes, &probe))
		generations = append(generations, probe.Generation)
		if probe.StopReason != "" {
			sawFinal = true
			assert.Equal(t, "stop", probe.StopReason)
		}
	}
	require.NoError(t, resp.GetStream().Err())

	assert.Equal(t, []string{"Hello", " world", ""}, generations)
	assert.True(t, sawFinal)
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}
