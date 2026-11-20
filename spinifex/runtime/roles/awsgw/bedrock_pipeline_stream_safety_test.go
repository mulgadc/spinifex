package awsgw_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	esv2 "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBedrockPipeline_InvokeStreamDecoderFaultIsSanitized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
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

	body := []byte(`{"prompt":"` + pipelinePromptMarker + `"}`)
	resp := doRequest(app, signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/invoke-with-response-stream", body))
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	var messages []string
	dec := esv2.NewDecoder()
	r := bytes.NewReader(raw)
	for r.Len() > 0 {
		msg, err := dec.Decode(r, nil)
		require.NoError(t, err)
		assert.NotContains(t, string(msg.Payload), pipelineUsageMarker)
		if msg.Headers.Get(":message-type").String() == "exception" {
			assert.Equal(t, "modelStreamErrorException", msg.Headers.Get(":exception-type").String())
			var payload struct {
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(msg.Payload, &payload))
			messages = append(messages, payload.Message)
		}
	}
	assert.Equal(t, []string{"The model stream ended with an error."}, messages)
	assert.Contains(t, logs.String(), "failure=unexpected_type")
	assert.NotContains(t, logs.String(), pipelineUsageMarker)
	assert.NotContains(t, logs.String(), pipelinePromptMarker)
}
