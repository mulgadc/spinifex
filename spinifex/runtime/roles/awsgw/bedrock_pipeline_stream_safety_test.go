package awsgw_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

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

// readyEndpoints reports a single READY endpoint record for every lookup.
type readyEndpoints struct {
	record ochre.EndpointRecord
}

var _ ochre.EndpointService = (*readyEndpoints)(nil)

func (e *readyEndpoints) Ensure(context.Context, *ochre.EnsureEndpointInput, string) (*ochre.EnsureEndpointOutput, error) {
	return &ochre.EnsureEndpointOutput{Endpoint: e.record}, nil
}

func (e *readyEndpoints) Describe(context.Context, *ochre.DescribeEndpointInput, string) (*ochre.DescribeEndpointOutput, error) {
	return &ochre.DescribeEndpointOutput{Endpoint: e.record}, nil
}

func (e *readyEndpoints) List(context.Context, *ochre.ListEndpointsInput, string) (*ochre.ListEndpointsOutput, error) {
	return &ochre.ListEndpointsOutput{Endpoints: []ochre.EndpointRecord{e.record}}, nil
}

func (e *readyEndpoints) Delete(context.Context, *ochre.DeleteEndpointInput, string) (*ochre.DeleteEndpointOutput, error) {
	return &ochre.DeleteEndpointOutput{}, nil
}

func TestBedrockPipeline_RedirectedReadyMemberIsNotReadyWithoutWaiting(t *testing.T) {
	t.Parallel()
	var sourceHits, destHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", destination.URL+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	u, err := url.Parse(source.URL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	svc := &readyEndpoints{record: ochre.EndpointRecord{
		ModelID:   bedrockPipelineLlamaModel,
		State:     ochre.StateReady,
		PrivateIP: host,
		Members:   map[string]ochre.MemberEndpoint{bedrockPipelineLlamaModel: {Port: port}},
	}}
	app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) {
		c.Deps.Endpoints = ochre.NewDynamicEndpointResolver(svc, nil, 0, ochre.WithColdStartWait(time.Minute))
	}, allowBedrock())

	start := time.Now()
	got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse", pipelineConverseBody(pipelinePromptMarker)))

	assert.Less(t, time.Since(start), 10*time.Second, "a redirected READY member must not wait out the cold-start budget")
	assert.Equal(t, http.StatusTooManyRequests, got.Status, got.Body)
	assert.Equal(t, "ModelNotReadyException", got.Type, got.Body)
	assert.EqualValues(t, 1, sourceHits.Load(), "the readiness probe never reached the member")
	assert.Zero(t, destHits.Load(), "the readiness probe followed the redirect")
}
