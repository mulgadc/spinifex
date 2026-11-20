package ochre

//test:in-package: drives the unexported resolver probe, provider clients and
// stream handlers against local servers while capturing process logs.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log" //nolint:depguard // the HTTP transport writes stray response bytes through the standard logger
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	safetyEndpointUser = "endpoint-user-5510"
	safetyEndpointPass = "endpoint-pass-8812"
	safetyEndpointPath = "endpoint-path-3391"
	safetyStrayMarker  = "stray-marker-6620"
	safetyUsageMarker  = "92233720368547758079731"
)

type safetyLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safetyLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safetyLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureSafetyLogs(t *testing.T) *safetyLogBuffer {
	t.Helper()
	logs := &safetyLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

func safetyClosedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	require.NoError(t, ln.Close())
	return port
}

// redirectingMember is a READY member whose readiness route redirects to a
// healthy destination; it counts hits at both.
func redirectingMember(t *testing.T, location func(dest string) string) (*fakeEndpointService, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	destHits, sourceHits := &atomic.Int32{}, &atomic.Int32{}
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sourceHits.Add(1)
		w.Header().Set("Location", location(dest.URL))
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(src.Close)
	return &fakeEndpointService{record: memberReadyRecord(t, src)}, sourceHits, destHits
}

func TestDynamicEndpointResolver_RedirectedReadyMemberIsNotReadyWithoutWaiting(t *testing.T) {
	t.Parallel()
	cases := map[string]func(dest string) string{
		"redirect":           func(dest string) string { return dest + "/health" },
		"malformed location": func(string) string { return "http://[::1" },
	}
	for name, location := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, sourceHits, destHits := redirectingMember(t, location)
			r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(time.Minute))

			start := time.Now()
			baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
			require.NoError(t, err)
			assert.False(t, ok)
			assert.Empty(t, baseURL)
			assert.Less(t, time.Since(start), coldStartPollInterval, "a redirected READY member must not wait for readiness")
			assert.EqualValues(t, 1, sourceHits.Load())
			assert.Zero(t, destHits.Load(), "the readiness probe followed the redirect")
		})
	}
}

func TestDynamicEndpointResolver_RedirectedOutcomeIsCached(t *testing.T) {
	t.Parallel()
	svc, sourceHits, destHits := redirectingMember(t, func(dest string) string { return dest })
	r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(time.Minute), WithLivenessCacheTTL(time.Minute))

	for range 3 {
		_, ok, err := r.Endpoint(context.Background(), resolverTestModel)
		require.NoError(t, err)
		assert.False(t, ok)
	}
	assert.EqualValues(t, 1, sourceHits.Load(), "a cached redirect outcome must not re-probe")
	assert.Zero(t, destHits.Load())
}

func TestDynamicEndpointResolver_NotLiveReadyMemberStillWaits(t *testing.T) {
	t.Parallel()
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	svc := &fakeEndpointService{record: memberReadyRecord(t, srv)}
	r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(time.Minute), WithLivenessCacheTTL(time.Nanosecond))

	baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	require.True(t, ok, "an ordinary not-live member must be polled until it answers")
	assert.Equal(t, srv.URL, baseURL)
	assert.GreaterOrEqual(t, probes.Load(), int32(2))
}

// countingTransport counts round trips through the injected client's transport.
type countingTransport struct {
	calls atomic.Int32
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return http.DefaultTransport.RoundTrip(req)
}

func TestProbeLiveness_KeepsInjectedClientBehaviourAndLeavesItUnchanged(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		cookie, err := r.Cookie("probe")
		if !ok || user != safetyEndpointUser || pass != safetyEndpointPass || err != nil || cookie.Value != "jar" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL + "/health")
	require.NoError(t, err)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(u, []*http.Cookie{{Name: "probe", Value: "jar"}})
	transport := &countingTransport{}
	client := &http.Client{Transport: transport, Jar: jar}
	u.User = url.UserPassword(safetyEndpointUser, safetyEndpointPass)

	assert.Equal(t, livenessLive, probeLiveness(context.Background(), client, u.String()))
	assert.EqualValues(t, 1, transport.calls.Load(), "the probe bypassed the injected transport")
	assert.Same(t, transport, client.Transport.(*countingTransport), "the probe replaced the injected transport")
	assert.Nil(t, client.CheckRedirect, "the probe mutated the injected client's redirect policy")
}

func TestProbeLiveness_KeepsInjectedClientTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	client := &http.Client{Timeout: 20 * time.Millisecond}

	start := time.Now()
	assert.Equal(t, livenessNotLive, probeLiveness(context.Background(), client, srv.URL))
	assert.Less(t, time.Since(start), livenessProbeTimeout, "the probe ignored the injected client's timeout")
}

func TestProviderClients_TransportFailureLogsOmitEndpointURL(t *testing.T) {
	logs := captureSafetyLogs(t)
	baseURL := "http://" + safetyEndpointUser + ":" + safetyEndpointPass + "@127.0.0.1:" + safetyClosedPort(t) + "/" + safetyEndpointPath
	for name, call := range providerCalls() {
		t.Run(name, func(t *testing.T) {
			require.Error(t, call(baseURL))
		})
	}
	out := logs.String()
	require.Contains(t, out, "failure=unreachable", "the transport failures were not classified")
	for _, marker := range []string{safetyEndpointUser, safetyEndpointPass, safetyEndpointPath, reproAPIKey, reproPromptMarker} {
		assert.NotContains(t, out, marker)
	}
}

func TestProviderClients_DecodeFailureLogsOmitUpstreamValue(t *testing.T) {
	logs := captureSafetyLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":` + safetyUsageMarker + `,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	for _, name := range []string{"vllm converse", "llama invoke"} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, providerCalls()[name](srv.URL))
		})
	}
	out := logs.String()
	require.Contains(t, out, "unexpected_type", "the decode failures were not classified")
	assert.NotContains(t, out, safetyUsageMarker)
}

// strayByteServer answers once with a complete response followed by stray
// bytes, then reports when the client closes the connection.
func strayByteServer(t *testing.T, status int, body string) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s%s",
			status, http.StatusText(status), len(body), body, safetyStrayMarker)
		_ = buf.Flush()
		_, _ = io.Copy(io.Discard, bufio.NewReader(conn))
		close(closed)
	}))
	t.Cleanup(srv.Close)
	return srv, closed
}

func captureStdLog(t *testing.T) *safetyLogBuffer {
	t.Helper()
	logs := &safetyLogBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	return logs
}

func TestProviderClients_StrayResponseBytesAreNotLogged(t *testing.T) {
	stdLogs := captureStdLog(t)
	cases := map[string]struct {
		status int
		body   string
		call   func(baseURL string) error
	}{
		"vllm converse": {
			status: http.StatusOK,
			body:   `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			call:   func(u string) error { return providerCalls()["vllm converse"](u) },
		},
		"tei invoke": {
			status: http.StatusRequestEntityTooLarge,
			call: func(u string) error {
				a := newTEIEmbeddingInvokeAdapter(NewStaticEndpointResolver(map[string]string{nomicModel: u}))
				_, _, err := a.InvokeModel(context.Background(), nomicModel, []byte(`{"input":"hi"}`))
				return err
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, closed := strayByteServer(t, tc.status, tc.body)
			err := tc.call(srv.URL)
			if tc.status == http.StatusOK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("the client kept the connection open after the response")
			}
		})
	}
	assert.NotContains(t, stdLogs.String(), safetyStrayMarker)
}

func TestStreamHandlers_AnthropicErrorEventIsSanitizedInFramesAndLogs(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"bad key " + reproEchoSecret + "\"}}\n\n"))
	}))
	defer upstream.Close()
	dials := withLocalAnthropicTransport(t, upstream)
	withProviderCatalogEntry(t, safetyAnthropicModel)
	logs := captureSafetyLogs(t)
	creds := stubCredentialResolver{key: reproAPIKey, ok: true}

	ops := map[string]func(w http.ResponseWriter) error{
		"converse-stream": func(w http.ResponseWriter) error {
			body := []byte(`{"messages":[{"role":"user","content":[{"text":"` + reproPromptMarker + `"}]}]}`)
			return ConverseStream(context.Background(), w, "000000000001", safetyAnthropicModel, body, creds, nil, nil, grantAll{}, nil, nil, nil)
		},
		"invoke-with-response-stream": func(w http.ResponseWriter) error {
			body := []byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`)
			return InvokeModelWithResponseStream(context.Background(), w, "000000000001", safetyAnthropicModel, body,
				creds, nil, "application/json", nil, grantAll{}, nil, "", "", nil, nil)
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			require.NoError(t, op(rec))
			var messages []string
			for _, f := range decodeAllFrames(t, rec.Body.Bytes()) {
				assert.NotContains(t, string(f.Payload), reproEchoSecret)
				if f.MessageType == "exception" {
					assert.Equal(t, excModelStreamErrorException, f.Type)
					var body struct {
						Message string `json:"message"`
					}
					require.NoError(t, json.Unmarshal(f.Payload, &body))
					messages = append(messages, body.Message)
				}
			}
			assert.Equal(t, []string{msgModelStreamError}, messages)
		})
	}
	assert.EqualValues(t, 2, dials.Load())
	out := logs.String()
	require.Contains(t, out, "upstream_error_event", "the stream faults were not classified")
	for _, marker := range []string{reproEchoSecret, reproAPIKey, reproPromptMarker} {
		assert.NotContains(t, out, marker)
	}
	assert.NotContains(t, out, "overloaded_error", "the provider's error type was logged")
}
