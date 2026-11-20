package ochre_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	kbEmbedModel  = "nomic-embed-text-v1.5"
	kbRerankModel = "bge-reranker-base"
)

var redirectStatuses = []int{
	http.StatusMovedPermanently,
	http.StatusFound,
	http.StatusSeeOther,
	http.StatusTemporaryRedirect,
	http.StatusPermanentRedirect,
}

// redirectPair returns a source server that redirects every request to a
// separate destination server, and counters for the requests each received.
// reply, when set, answers at the destination; nil answers an empty 200.
func redirectPair(t *testing.T, status int, reply http.HandlerFunc) (sourceURL string, sourceCalls, destinationCalls *atomic.Int32) {
	t.Helper()
	sourceCalls, destinationCalls = new(atomic.Int32), new(atomic.Int32)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		if reply != nil {
			reply(w, r)
		}
	}))
	t.Cleanup(destination.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		http.Redirect(w, r, destination.URL+r.URL.Path, status)
	}))
	t.Cleanup(source.Close)
	return source.URL, sourceCalls, destinationCalls
}

// countingServer answers every request with handler and counts them.
func countingServer(t *testing.T, handler http.HandlerFunc) (string, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, calls
}

func kbEmbedder(baseURL string) ochre.Embedder {
	return ochre.NewEmbedder(ochre.NewStaticEndpointResolver(map[string]string{kbEmbedModel: baseURL}))
}

func kbReranker(baseURL string) ochre.Reranker {
	return ochre.NewReranker(ochre.NewStaticEndpointResolver(map[string]string{kbRerankModel: baseURL}), kbRerankModel)
}

func TestKBEmbedder_DoesNotFollowRedirects(t *testing.T) {
	for _, status := range redirectStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			sourceURL, sourceCalls, destinationCalls := redirectPair(t, status, nil)

			_, err := kbEmbedder(sourceURL).Embed(context.Background(), kbEmbedModel, []string{"search_document: tenant text"})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
			assert.Zero(t, destinationCalls.Load(), "the redirect target must receive nothing")
			assert.Equal(t, int32(1), sourceCalls.Load(), "a redirect must not be retried")
		})
	}
}

func TestKBEmbedder_TokenizeDoesNotFollowRedirects(t *testing.T) {
	for _, status := range redirectStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			sourceURL, sourceCalls, destinationCalls := redirectPair(t, status, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`[{"id":1},{"id":2}]`))
			})
			limiter, ok := kbEmbedder(sourceURL).(ochre.TokenLimiter)
			require.True(t, ok)

			_, counted := limiter.CountTokens(context.Background(), kbEmbedModel, "tenant text")
			assert.False(t, counted, "a redirected tokenize call must fall back to the caller's estimate")
			assert.Zero(t, destinationCalls.Load(), "the redirect target must receive nothing")
			assert.Equal(t, int32(1), sourceCalls.Load())
		})
	}
}

func TestKBEmbedder_InfoRedirectFallsBackToDefault(t *testing.T) {
	unresolved, ok := ochre.NewEmbedder(ochre.NewStaticEndpointResolver(nil)).(ochre.TokenLimiter)
	require.True(t, ok)
	fallback := unresolved.MaxInputLength(context.Background(), kbEmbedModel)

	sourceURL, sourceCalls, destinationCalls := redirectPair(t, http.StatusTemporaryRedirect, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"max_input_length":8192}`))
	})
	limiter, ok := kbEmbedder(sourceURL).(ochre.TokenLimiter)
	require.True(t, ok)

	assert.Equal(t, fallback, limiter.MaxInputLength(context.Background(), kbEmbedModel))
	assert.Equal(t, int32(1), sourceCalls.Load(), "the configured endpoint must be asked for /info once")
	assert.Zero(t, destinationCalls.Load(), "the redirect target must not be asked for /info")
}

func TestKBEmbedder_InfoServedLimitOverridesDefault(t *testing.T) {
	var infoCalls atomic.Int32
	sourceURL, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/info" {
			infoCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{"max_input_length":8192}`))
	})
	limiter, ok := kbEmbedder(sourceURL).(ochre.TokenLimiter)
	require.True(t, ok)

	assert.Equal(t, 8192, limiter.MaxInputLength(context.Background(), kbEmbedModel))
	assert.Equal(t, int32(1), infoCalls.Load())
}

// failingResolver fails every lookup with err.
type failingResolver struct{ err error }

func (r failingResolver) Endpoint(context.Context, string) (string, bool, error) {
	return "", false, r.err
}

func (r failingResolver) EndpointForAccount(context.Context, string, string) (string, bool, error) {
	return "", false, r.err
}

var _ ochre.EndpointResolver = failingResolver{}

func TestKBProviders_ResolverErrorLogOmitsDetail(t *testing.T) {
	resolver := failingResolver{err: errors.New("lookup http://kbuser:kbsecret@10.0.0.9:8080 failed")}
	logs := captureDebugLogs(t)

	_, embedErr := ochre.NewEmbedder(resolver).Embed(context.Background(), kbEmbedModel, []string{"tenant text"})
	_, rerankErr := ochre.NewReranker(resolver, kbRerankModel).Rerank(context.Background(), "tenant query", []string{"tenant doc"})
	require.Error(t, embedErr)
	require.Error(t, rerankErr)

	out := logs.String()
	assert.Contains(t, out, "embeddings: endpoint resolution failed")
	assert.Contains(t, out, "rerank: endpoint resolution failed")
	for _, secret := range []string{"kbuser", "kbsecret", "10.0.0.9"} {
		assert.NotContains(t, out, secret)
	}
}

// overrunServer answers each request with a response whose body runs past its
// declared Content-Length by trailer, and closes connClosed once the client
// closes its connection.
func overrunServer(t *testing.T, body, trailer string) (baseURL string, connClosed <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	closed := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(closed)
		br := bufio.NewReader(conn)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s%s", len(body), body, trailer)
		}
	}()
	return "http://" + ln.Addr().String(), closed
}

// TestKBProviders_StrayResponseBytesNotLogged: Go's transport logs bytes that
// arrive on an idle connection, so the KB clients must not keep one.
func TestKBProviders_StrayResponseBytesNotLogged(t *testing.T) {
	const marker = "tenant-marker-9931"
	cases := map[string]struct {
		body string
		call func(baseURL string) error
	}{
		"embed": {
			body: `{"data":[{"index":0,"embedding":[0.1]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`,
			call: func(baseURL string) error {
				_, err := kbEmbedder(baseURL).Embed(context.Background(), kbEmbedModel, []string{"tenant text"})
				return err
			},
		},
		"rerank": {
			body: `[{"index":0,"score":0.5}]`,
			call: func(baseURL string) error {
				_, err := kbReranker(baseURL).Rerank(context.Background(), "tenant query", []string{"tenant doc"})
				return err
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			baseURL, connClosed := overrunServer(t, tc.body, marker)
			logs := captureDebugLogs(t)

			require.NoError(t, tc.call(baseURL))
			select {
			case <-connClosed:
			case <-time.After(5 * time.Second):
				t.Fatal("the client kept its provider connection open")
			}
			assert.NotContains(t, logs.String(), marker)
		})
	}
}

func TestKBReranker_DoesNotFollowRedirects(t *testing.T) {
	for _, status := range redirectStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			sourceURL, sourceCalls, destinationCalls := redirectPair(t, status, nil)

			_, err := kbReranker(sourceURL).Rerank(context.Background(), "tenant query", []string{"tenant doc a", "tenant doc b"})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
			assert.Zero(t, destinationCalls.Load(), "the redirect target must receive nothing")
			assert.Equal(t, int32(1), sourceCalls.Load(), "a redirect must not be retried")
		})
	}
}

// TestKBProviders_RedirectLocationEdgeCases: a missing Location is still a
// non-success response, while a malformed one fails the request without
// being retried; neither is followed.
func TestKBProviders_RedirectLocationEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		location string
		wantCode string
		marker   string
	}{
		{name: "empty", location: "", wantCode: awserrors.ErrorModelErrorException},
		{name: "malformed", location: "http://[::1/loc-marker-5523", marker: "loc-marker-5523"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sourceURL, sourceCalls := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.WriteHeader(http.StatusTemporaryRedirect)
			})
			logs := captureDebugLogs(t)

			_, embedErr := kbEmbedder(sourceURL).Embed(context.Background(), kbEmbedModel, []string{"tenant text"})
			_, rerankErr := kbReranker(sourceURL).Rerank(context.Background(), "tenant query", []string{"tenant doc"})
			require.Error(t, embedErr)
			require.Error(t, rerankErr)
			if tc.wantCode != "" {
				assert.Equal(t, tc.wantCode, embedErr.Error())
				assert.Equal(t, tc.wantCode, rerankErr.Error())
			}
			assert.Equal(t, int32(2), sourceCalls.Load(), "each provider must call the source once, without retry")
			if tc.marker != "" {
				assert.Contains(t, logs.String(), "endpoint=", "the request failure must still be logged")
				assert.NotContains(t, logs.String(), tc.marker, "logs must not quote the upstream Location")
			}
		})
	}
}

// TestKBProviders_UnreadableNonSuccessBodyKeepsMappedStatus: the status is
// mapped before the body is read, so a truncated error or redirect body
// cannot turn the mapped failure into ServiceUnavailable.
func TestKBProviders_UnreadableNonSuccessBodyKeepsMappedStatus(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			sourceURL, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "/elsewhere")
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("partial"))
			})

			_, embedErr := kbEmbedder(sourceURL).Embed(context.Background(), kbEmbedModel, []string{"tenant text"})
			_, rerankErr := kbReranker(sourceURL).Rerank(context.Background(), "tenant query", []string{"tenant doc"})
			require.Error(t, embedErr)
			require.Error(t, rerankErr)
			assert.Equal(t, awserrors.ErrorModelErrorException, embedErr.Error())
			assert.Equal(t, awserrors.ErrorModelErrorException, rerankErr.Error())
		})
	}
}

// lockedBuffer is a log sink safe for handlers writing from other goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureDebugLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// closedPort returns a loopback port with nothing listening on it.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	require.NoError(t, ln.Close())
	return port
}

// exerciseKBProviders drives every embedder and reranker request path once
// against baseURL, ignoring the results.
func exerciseKBProviders(t *testing.T, baseURL string) {
	t.Helper()
	ctx := context.Background()
	embedder := kbEmbedder(baseURL)
	limiter, ok := embedder.(ochre.TokenLimiter)
	require.True(t, ok)
	_, _ = embedder.Embed(ctx, kbEmbedModel, []string{"tenant text"})
	_, _ = limiter.CountTokens(ctx, kbEmbedModel, "tenant text")
	_ = limiter.MaxInputLength(ctx, kbEmbedModel)
	_, _ = kbReranker(baseURL).Rerank(ctx, "tenant query", []string{"tenant doc"})
}

func TestKBProviders_LogsOmitURLCredentials(t *testing.T) {
	for name, baseURL := range map[string]string{
		"transport failure":    "http://kbuser:kbsecret@127.0.0.1:" + closedPort(t) + "/path?token=kbquery",
		"request construction": "http://kbuser:kbsecret@bad host",
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureDebugLogs(t)
			exerciseKBProviders(t, baseURL)

			out := logs.String()
			assert.Contains(t, out, "endpoint=", "the failure must still be logged")
			for _, secret := range []string{"kbuser", "kbsecret", "kbquery"} {
				assert.NotContains(t, out, secret)
			}
		})
	}
}

func TestKBProviders_LogsOmitUpstreamContent(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		marker string
	}{
		"echoed error body":         {status: http.StatusUnprocessableEntity, body: `{"error":"tenant-marker-7781 is too long"}`, marker: "tenant-marker-7781"},
		"malformed success literal": {status: http.StatusOK, body: `{"data":[{"embedding":[1e999],"index":0}]}`, marker: "1e999"},
		"malformed list literal":    {status: http.StatusOK, body: `[{"index":0,"score":1e999,"id":1e999}]`, marker: "1e999"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sourceURL, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			logs := captureDebugLogs(t)
			exerciseKBProviders(t, sourceURL)

			out := logs.String()
			assert.Contains(t, out, "embeddings:", "the failure must still be logged")
			assert.NotContains(t, out, tc.marker)
		})
	}
}
