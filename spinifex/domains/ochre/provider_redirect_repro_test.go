package ochre

//test:in-package: drives the unexported provider constructors, the resolver's
// default probe client and the stream fault writer.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	reproPromptMarker = "prompt-marker-4417"
	reproAPIKey       = "sk-synthetic-key-7731"
	reproStreamMarker = "stream-marker-2286"
	reproEchoSecret   = "sk-echoed-secret-6043"
)

// redirectPair starts a destination and a source that answers every request
// with status and a Location on the destination under a different hostname
// (localhost against the source's 127.0.0.1), counting requests at both.
func redirectPair(t *testing.T, status int, dest http.HandlerFunc) (source *httptest.Server, sourceHits, hits *atomic.Int32) {
	t.Helper()
	sourceHits, hits = &atomic.Int32{}, &atomic.Int32{}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		dest(w, r)
	}))
	t.Cleanup(destination.Close)
	crossHost := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1)
	require.NotEqual(t, destination.URL, crossHost)
	source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", crossHost+r.URL.Path)
		w.WriteHeader(status)
	}))
	t.Cleanup(source.Close)
	return source, sourceHits, hits
}

func TestVLLMProvider_ConverseRedirectDoesNotForwardPrompt(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var forwarded atomic.Bool
			source, sourceHits, hits := redirectPair(t, status, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				forwarded.Store(bytes.Contains(body, []byte(reproPromptMarker)))
				w.WriteHeader(http.StatusOK)
			})
			modelID := "meta.llama3-2-1b-instruct-v1:0"
			p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: source.URL}))

			_, _ = p.Converse(context.Background(), modelID, &bedrockruntime.ConverseInput{
				Messages: []*bedrockruntime.Message{{
					Role:    aws.String(bedrockruntime.ConversationRoleUser),
					Content: []*bedrockruntime.ContentBlock{{Text: aws.String(reproPromptMarker)}},
				}},
			})

			require.EqualValues(t, 1, sourceHits.Load(), "the provider never reached the source")
			assert.False(t, forwarded.Load(), "redirect target received the prompt")
			assert.Zero(t, hits.Load(), "redirect target received a request")
		})
	}
}

func TestAnthropicProvider_ConverseRedirectDoesNotForwardAPIKey(t *testing.T) {
	var gotKey, gotHost atomic.Value
	gotKey.Store("")
	gotHost.Store("")
	source, sourceHits, hits := redirectPair(t, http.StatusTemporaryRedirect, func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("X-Api-Key"))
		gotHost.Store(r.Host)
		w.WriteHeader(http.StatusOK)
	})
	p := newAnthropicProvider(reproAPIKey)
	bound, ok := p.(*anthropicBoundProvider)
	require.True(t, ok)
	bound.inner.baseURL = source.URL

	_, _ = p.Converse(context.Background(), "anthropic.claude-3-5-sonnet-20240620-v1:0", &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{{
			Role:    aws.String(bedrockruntime.ConversationRoleUser),
			Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}},
		}},
	})

	require.EqualValues(t, 1, sourceHits.Load(), "the provider never reached the source")
	t.Logf("source host %s, destination saw host %q", strings.TrimPrefix(source.URL, "http://"), gotHost.Load())
	assert.NotEqual(t, reproAPIKey, gotKey.Load(), "redirect target received the API key")
	assert.Zero(t, hits.Load(), "redirect target received a request")
}

func TestResolverLiveness_RedirectToHealthyServerIsNotLive(t *testing.T) {
	source, sourceHits, hits := redirectPair(t, http.StatusFound, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	modelID := "meta.llama3-2-1b-instruct-v1:0"
	r := NewDynamicEndpointResolver(nil, nil, 0)
	rec := EndpointRecord{Members: map[string]MemberEndpoint{modelID: {}}}

	live := r.isLive(context.Background(), rec, modelID, source.URL)

	require.EqualValues(t, 1, sourceHits.Load(), "the provider never reached the source")
	assert.False(t, live, "a redirected health probe marked the endpoint live")
	assert.Zero(t, hits.Load(), "the health probe followed the redirect")
}

func TestStreamUpstreamFault_LogOmitsUpstreamErrorEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"" + reproStreamMarker + "\"}}\n\n"))
	}))
	defer upstream.Close()
	p := newAnthropicProviderClient()
	p.baseURL = upstream.URL
	modelID := "anthropic.claude-3-5-sonnet-20240620-v1:0"
	src, err := p.ConverseStream(context.Background(), modelID, &bedrockruntime.ConverseStreamInput{
		Messages: []*bedrockruntime.Message{{
			Role:    aws.String(bedrockruntime.ConversationRoleUser),
			Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hi")}},
		}},
	}, reproAPIKey)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()
	_, _, faultErr := src.Next(context.Background())
	require.Error(t, faultErr)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)
	fw.writeUpstreamFault("converse-stream", modelID, faultErr)

	t.Logf("in-band frame carries upstream text: %v; carries API key: %v",
		strings.Contains(rec.Body.String(), reproStreamMarker), strings.Contains(rec.Body.String(), reproAPIKey))
	assert.NotContains(t, logs.String(), reproStreamMarker, "the fault log quotes the upstream error event")
	assert.NotContains(t, logs.String(), reproAPIKey)
}

// anthropicHostTransport sends every request for the Anthropic API host to
// target instead, leaving the production client and base URL untouched.
type anthropicHostTransport struct {
	target *url.URL
	next   http.RoundTripper
}

func (a anthropicHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "api.anthropic.com" {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = a.target.Scheme, a.target.Host
	}
	return a.next.RoundTrip(r)
}

func TestInvokeModelWithResponseStream_AnthropicErrorEventDoesNotLeakSecret(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"bad key " + reproEchoSecret + "\"}}\n\n"))
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	prev := http.DefaultTransport
	http.DefaultTransport = anthropicHostTransport{target: target, next: prev}
	defer func() { http.DefaultTransport = prev }()
	modelID := "anthropic.claude-3-5-sonnet-20240620-v1:0"
	withProviderCatalogEntry(t, modelID)

	rec := httptest.NewRecorder()
	err = InvokeModelWithResponseStream(context.Background(), rec, "000000000001", modelID,
		[]byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`),
		stubCredentialResolver{key: reproAPIKey, ok: true}, nil, "application/json", nil, grantAll{}, nil, "", "", nil, nil)
	require.NoError(t, err)

	var exceptions []string
	for _, f := range decodeAllFrames(t, rec.Body.Bytes()) {
		payload := f.Payload
		if f.MessageType == "event" && f.Type == "chunk" {
			var part struct{ Bytes []byte }
			require.NoError(t, json.Unmarshal(f.Payload, &part))
			payload = part.Bytes
		}
		t.Logf("frame %s/%s: %s", f.MessageType, f.Type, payload)
		assert.NotContains(t, string(payload), reproEchoSecret, "a %s/%s frame carries the provider-echoed secret", f.MessageType, f.Type)
		assert.NotContains(t, string(payload), reproAPIKey)
		if f.MessageType == "exception" {
			exceptions = append(exceptions, f.Type)
		}
	}
	assert.Equal(t, []string{excModelStreamErrorException}, exceptions, "the provider error event did not end the stream as an exception")
}
