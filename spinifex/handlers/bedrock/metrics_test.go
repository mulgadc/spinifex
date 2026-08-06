package handlers_bedrock

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
