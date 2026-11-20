package awsgw_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	esv2 "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pipelinePromptMarker = "prompt-marker-4417"
	pipelineUsageMarker  = "92233720368547758079731"
)

func pipelineConverseBody(text string) []byte {
	return []byte(`{"messages":[{"role":"user","content":[{"text":"` + text + `"}]}]}`)
}

func TestBedrockPipeline_ConverseRedirectDoesNotForwardPrompt(t *testing.T) {
	var sourceHits, destHits atomic.Int32
	var forwarded atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		forwarded.Store(bytes.Contains(body, []byte(pipelinePromptMarker)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	}))
	defer destination.Close()
	crossHost := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1)
	require.NotEqual(t, destination.URL, crossHost)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", crossHost+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) {
		c.Deps.Endpoints = ochre.NewStaticEndpointResolver(map[string]string{bedrockPipelineLlamaModel: source.URL})
	}, allowBedrock())

	got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse", pipelineConverseBody(pipelinePromptMarker)))

	require.EqualValues(t, 1, sourceHits.Load(), "the request never reached the source endpoint")
	assert.Zero(t, destHits.Load(), "redirect target received a request")
	assert.False(t, forwarded.Load(), "redirect target received the prompt")
	assert.Equal(t, http.StatusFailedDependency, got.Status, got.Body)
	assert.Equal(t, "ModelErrorException", got.Type, got.Body)
}

func TestBedrockPipeline_ConverseStreamDecoderFaultOmitsUpstreamValue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":` + pipelineUsageMarker + `,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) {
		c.Deps.Endpoints = ochre.NewStaticEndpointResolver(map[string]string{bedrockPipelineLlamaModel: upstream.URL})
	}, allowBedrock())
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	resp := doRequest(app, signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse-stream", pipelineConverseBody("hi")))
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Equal(t, "application/vnd.amazon.eventstream", resp.Header.Get("Content-Type"))

	var exceptions []string
	dec := esv2.NewDecoder()
	r := bytes.NewReader(raw)
	for r.Len() > 0 {
		msg, err := dec.Decode(r, nil)
		require.NoError(t, err)
		mt := msg.Headers.Get(":message-type").String()
		t.Logf("frame %s: %s", mt, msg.Payload)
		if mt == "exception" {
			exceptions = append(exceptions, msg.Headers.Get(":exception-type").String())
			var body struct{ Message string }
			require.NoError(t, json.Unmarshal(msg.Payload, &body))
			assert.NotContains(t, body.Message, pipelineUsageMarker, "the exception frame quotes the upstream value")
		}
	}
	assert.Equal(t, []string{"modelStreamErrorException"}, exceptions)
	assert.NotContains(t, logs.String(), pipelineUsageMarker, "the fault log quotes the upstream value")
}
