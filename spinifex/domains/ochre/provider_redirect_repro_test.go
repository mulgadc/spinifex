package ochre

//test:in-package: drives the unexported provider constructors, the resolver's
// default probe client and the stream fault writer.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
)

// redirectPair starts a destination that counts requests and a source that
// answers every request with status and a Location on the destination.
func redirectPair(t *testing.T, status int, dest http.HandlerFunc) (source *httptest.Server, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		dest(w, r)
	}))
	t.Cleanup(destination.Close)
	source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", destination.URL+r.URL.Path)
		w.WriteHeader(status)
	}))
	t.Cleanup(source.Close)
	return source, hits
}

func TestVLLMProvider_ConverseRedirectDoesNotForwardPrompt(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var forwarded atomic.Bool
			source, hits := redirectPair(t, status, func(w http.ResponseWriter, r *http.Request) {
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

			assert.False(t, forwarded.Load(), "redirect target received the prompt")
			assert.Zero(t, hits.Load(), "redirect target received a request")
		})
	}
}

func TestAnthropicProvider_ConverseRedirectDoesNotForwardAPIKey(t *testing.T) {
	var gotKey atomic.Value
	gotKey.Store("")
	source, hits := redirectPair(t, http.StatusTemporaryRedirect, func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("X-Api-Key"))
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

	assert.NotEqual(t, reproAPIKey, gotKey.Load(), "redirect target received the API key")
	assert.Zero(t, hits.Load(), "redirect target received a request")
}

func TestResolverLiveness_RedirectToHealthyServerIsNotLive(t *testing.T) {
	source, hits := redirectPair(t, http.StatusFound, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	modelID := "meta.llama3-2-1b-instruct-v1:0"
	r := NewDynamicEndpointResolver(nil, nil, 0)
	rec := EndpointRecord{Members: map[string]MemberEndpoint{modelID: {}}}

	live := r.isLive(context.Background(), rec, modelID, source.URL)

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
