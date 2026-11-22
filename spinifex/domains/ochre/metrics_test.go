package ochre

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vLLM emits one line per served model, so a server hosting two models emits
// two of every series. Idleness is a property of the whole endpoint, so the
// parser must sum them rather than take the last one it saw.
func TestParseVLLMMetrics_SumsAcrossLabelSets(t *testing.T) {
	body := `# HELP vllm:num_requests_running Number of requests currently running.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="a"} 2.0
vllm:num_requests_running{model_name="b"} 3.0
vllm:num_requests_waiting{model_name="a"} 1.0
vllm:num_requests_waiting{model_name="b"} 0.0
vllm:request_success_total{finished_reason="stop",model_name="a"} 7.0
vllm:request_success_total{finished_reason="length",model_name="a"} 5.0
`
	m, err := parseVLLMMetrics(strings.NewReader(body))
	require.NoError(t, err)
	assert.InDelta(t, 5.0, m.running, 0)
	assert.InDelta(t, 1.0, m.waiting, 0)
	assert.InDelta(t, 12.0, m.successTotal, 0)
	assert.Equal(t, 6, m.inFlight())
}

func TestParseVLLMMetrics_UnlabelledSeries(t *testing.T) {
	body := "vllm:num_requests_running 0.0\nvllm:num_requests_waiting 0.0\nvllm:request_success_total 4\n"
	m, err := parseVLLMMetrics(strings.NewReader(body))
	require.NoError(t, err)
	assert.Zero(t, m.inFlight())
	assert.InDelta(t, 4.0, m.successTotal, 0)
}

// One unparseable line must not discard the rest of the scrape: the
// alternative is that a single odd series makes a healthy endpoint look
// unreachable and eventually gets it relaunched.
func TestParseVLLMMetrics_SkipsUnparseableLines(t *testing.T) {
	body := `# a comment

vllm:num_requests_running{model_name="a" 1.0
vllm:num_requests_waiting{model_name="a"} not-a-number
python_gc_objects_collected_total{generation="0"} 143.0
vllm:num_requests_running{model_name="b"} 4.0
`
	m, err := parseVLLMMetrics(strings.NewReader(body))
	require.NoError(t, err)
	assert.InDelta(t, 4.0, m.running, 0, "the well-formed line must still be counted")
	assert.Zero(t, m.waiting)
}

// A 200 carrying none of the three series is something other than the vLLM
// server we expected, which is a failed scrape, not an idle endpoint.
func TestParseVLLMMetrics_NoKnownSeriesIsError(t *testing.T) {
	_, err := parseVLLMMetrics(strings.NewReader("go_goroutines 12\n"))
	require.Error(t, err)
}

func TestScrapeMetrics_ReadsLiveEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics", r.URL.Path)
		_, _ = w.Write([]byte("vllm:num_requests_running 1.0\nvllm:num_requests_waiting 2.0\nvllm:request_success_total 9\n"))
	}))
	defer srv.Close()

	m, err := scrapeMetrics(t.Context(), srv.Client(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 3, m.inFlight())
	assert.InDelta(t, 9.0, m.successTotal, 0)
}

func TestScrapeMetrics_Non200IsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := scrapeMetrics(t.Context(), srv.Client(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

// The counter clause is the point of the definition: a request that started
// and finished entirely between two sweeps leaves both queues at zero, and
// only the advanced counter says it happened.
func TestEndpointMetrics_Idle(t *testing.T) {
	tests := []struct {
		name    string
		sample  endpointMetrics
		prev    float64
		wantIdl bool
	}{
		{"empty queues and unchanged counter", endpointMetrics{successTotal: 4}, 4, true},
		{"counter advanced", endpointMetrics{successTotal: 5}, 4, false},
		{"request running", endpointMetrics{running: 1, successTotal: 4}, 4, false},
		{"request queued", endpointMetrics{waiting: 1, successTotal: 4}, 4, false},
		{"first sweep of a never-used endpoint", endpointMetrics{}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantIdl, tt.sample.idle(tt.prev))
		})
	}
}

const probeMetricsBody = "vllm:num_requests_running 1.0\nvllm:num_requests_waiting 2.0\nvllm:request_success_total 9\n"

// metricsTarget is a healthy metrics server a redirect could point at; it
// counts every request it is sent.
func metricsTarget(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(probeMetricsBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestProbeMetrics_ReadsLiveEndpoint(t *testing.T) {
	srv, hits := metricsTarget(t)
	m, err := probeMetrics(t.Context(), srv.Client(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 3, m.inFlight())
	assert.InDelta(t, 9.0, m.successTotal, 0)
	assert.EqualValues(t, 1, hits.Load())
}

func TestProbeMetrics_OrdinaryFailuresAreNotRedirects(t *testing.T) {
	for _, status := range []int{http.StatusNotModified, http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			_, err := probeMetrics(t.Context(), srv.Client(), srv.URL)
			require.Error(t, err)
			assert.NotErrorIs(t, err, errMetricsRedirected)
			assert.Contains(t, err.Error(), "HTTP "+strconv.Itoa(status))
		})
	}
}

func TestProbeMetrics_RedirectIsNeverFollowed(t *testing.T) {
	target, hits := metricsTarget(t)
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			location := target.URL + "/metrics?locsecret=1"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", location)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			_, err := probeMetrics(t.Context(), srv.Client(), srv.URL)
			require.ErrorIs(t, err, errMetricsRedirected)
			assert.NotContains(t, err.Error(), "locsecret")
			assert.NotContains(t, err.Error(), target.URL)
		})
	}
	assert.Zero(t, hits.Load(), "the redirect target was sent a request")
}

func TestProbeMetrics_MalformedOrMissingLocationIsARedirect(t *testing.T) {
	for name, location := range map[string]string{
		"unparseable": "http://[::1-locsecret",
		"bad escape":  "/metrics%zzlocsecret",
		"missing":     "",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if location != "" {
					w.Header().Set("Location", location)
				}
				w.WriteHeader(http.StatusFound)
			}))
			defer srv.Close()

			_, err := probeMetrics(t.Context(), srv.Client(), srv.URL)
			require.ErrorIs(t, err, errMetricsRedirected)
			assert.NotContains(t, err.Error(), "locsecret", "the Location is never quoted")
		})
	}
}

func TestProbeMetrics_KeepsInjectedClientAndLeavesItUnchanged(t *testing.T) {
	target, hits := metricsTarget(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/metrics", http.StatusFound)
	}))
	defer srv.Close()
	transport := &countingTransport{}
	followed := false
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		followed = true
		return nil
	}}

	_, err := probeMetrics(t.Context(), client, srv.URL)
	require.ErrorIs(t, err, errMetricsRedirected, "an injected following policy is not used")
	assert.False(t, followed)
	assert.Zero(t, hits.Load())
	assert.EqualValues(t, 1, transport.calls.Load(), "the probe bypassed the injected transport")
	assert.Same(t, transport, client.Transport.(*countingTransport), "the probe replaced the injected transport")
	require.NotNil(t, client.CheckRedirect, "the probe mutated the injected client's redirect policy")

	m, err := probeMetrics(t.Context(), client, target.URL)
	require.NoError(t, err, "a 200 through the injected client still reads")
	assert.Equal(t, 3, m.inFlight())
	assert.EqualValues(t, 2, transport.calls.Load())
	assert.False(t, followed)
}

func TestProbeMetrics_FailureOmitsEndpointCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	u.User = url.UserPassword(safetyEndpointUser, safetyEndpointPass)
	u.Path = "/" + safetyEndpointPath

	_, err = probeMetrics(t.Context(), srv.Client(), u.String())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), safetyEndpointPass)
	assert.NotContains(t, err.Error(), safetyEndpointPath)

	closed := "http://" + safetyEndpointUser + ":" + safetyEndpointPass + "@127.0.0.1:" + safetyClosedPort(t) + "/" + safetyEndpointPath
	_, err = probeMetrics(t.Context(), srv.Client(), closed)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errMetricsRedirected)
	assert.NotContains(t, err.Error(), safetyEndpointPass)
	assert.NotContains(t, err.Error(), safetyEndpointPath)
}
