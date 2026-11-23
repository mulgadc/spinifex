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

// vllmSample is one exposition line in the label order vLLM 0.26 prints.
func vllmSample(name, model, engine, reason, value string) string {
	labels := `model_name="` + model + `",engine="` + engine + `"`
	if reason != "" {
		labels += `,finished_reason="` + reason + `"`
	}
	return name + "{" + labels + "} " + value
}

// vllmMetricsLines is a complete vLLM 0.26 scrape for model on engine 0, with
// every completed request counted as "stop", among the unrelated series and
// metadata a real server also exports.
func vllmMetricsLines(model string, running, waiting int, successTotal uint64) []string {
	lines := []string{
		"# HELP vllm:num_requests_running Number of requests in model execution batches.",
		"# TYPE vllm:num_requests_running gauge",
		vllmSample(metricRunning, model, "0", "", strconv.Itoa(running)+".0"),
		"# TYPE vllm:num_requests_waiting gauge",
		vllmSample(metricWaiting, model, "0", "", strconv.Itoa(waiting)+".0"),
		`vllm:num_requests_waiting_by_reason{model_name="` + model + `",engine="0",reason="capacity"} 3.0`,
		"# TYPE vllm:request_success counter",
	}
	for _, reason := range vllmMetricsFinishReasons {
		value := "0.0"
		if reason == "stop" {
			value = strconv.FormatUint(successTotal, 10) + ".0"
		}
		lines = append(lines, vllmSample(metricSuccessTotal, model, "0", reason, value))
		lines = append(lines, vllmSample("vllm:request_success_created", model, "0", reason, "1.7634e+09"))
	}
	return append(lines, `python_gc_objects_collected_total{generation="0"} 143.0`)
}

func vllmMetricsBody(model string, running, waiting int, successTotal uint64) string {
	return strings.Join(vllmMetricsLines(model, running, waiting, successTotal), "\n") + "\n"
}

// editLines replaces each line starting with a key of edits by its value,
// dropping it when the value is empty.
func editLines(lines []string, edits map[string]string) string {
	var out []string
	for _, line := range lines {
		replaced := false
		for prefix, with := range edits {
			if strings.HasPrefix(line, prefix) {
				replaced = true
				if with != "" {
					out = append(out, with)
				}
			}
		}
		if !replaced {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

func successLine(reason string) string {
	return metricSuccessTotal + `{model_name="` + testModelID + `",engine="0",finished_reason="` + reason + `"}`
}

func gaugeLine(name string) string {
	return name + `{model_name="` + testModelID + `",engine="0"}`
}

// The complete scrape sums each family across finish reasons and ignores
// unrelated series, including those sharing a prefix with a required one.
func TestParseVLLMMetrics_CompleteSample(t *testing.T) {
	lines := vllmMetricsLines(testModelID, 2, 1, 0)
	body := editLines(lines, map[string]string{
		successLine("stop"):   successLine("stop") + " 7.0",
		successLine("length"): successLine("length") + " 1.234567e+06",
		successLine("abort"):  successLine("abort") + " 5",
	})
	m, err := parseVLLMMetrics([]byte(body), testModelID)
	require.NoError(t, err)
	assert.Equal(t, endpointMetrics{running: 2, waiting: 1, successTotal: 1234579}, m)
	assert.Equal(t, 3, m.inFlight())
}

func TestParseVLLMMetrics_AcceptsEscapedLabelValues(t *testing.T) {
	model := `odd"model\name`
	escaped := `odd\"model\\name`
	body := strings.ReplaceAll(vllmMetricsBody("MODEL", 0, 0, 4), `"MODEL"`, `"`+escaped+`"`)
	m, err := parseVLLMMetrics([]byte(body), model)
	require.NoError(t, err)
	assert.EqualValues(t, 4, m.successTotal)
}

// Every incomplete, mislabelled, duplicated or malformed required sample fails
// the whole scrape, however plausible the rest of it is.
func TestParseVLLMMetrics_RejectsIncompleteOrInvalidSamples(t *testing.T) {
	maxCount := strconv.FormatUint(maxExactCount, 10)
	cases := map[string]struct {
		body string
		want error
	}{
		"running missing":  {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): ""}), errMetricsIncomplete},
		"waiting missing":  {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricWaiting): ""}), errMetricsIncomplete},
		"success missing":  {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{metricSuccessTotal + "{": ""}), errMetricsIncomplete},
		"no vLLM series":   {"go_goroutines 12\n", errMetricsIncomplete},
		"empty body":       {"", errMetricsIncomplete},
		"wrong model":      {vllmMetricsBody("other.model-v1:0", 0, 0, 0), errMetricsUnexpected},
		"unexpected engine": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			gaugeLine(metricRunning): vllmSample(metricRunning, testModelID, "1", "", "0.0"),
		}), errMetricsUnexpected},
		"engine omitted": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			gaugeLine(metricRunning): metricRunning + `{model_name="` + testModelID + `"} 0.0`,
		}), errMetricsUnexpected},
		"unlabelled": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			gaugeLine(metricWaiting): metricWaiting + " 0.0",
		}), errMetricsUnexpected},
		"extra label": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			gaugeLine(metricWaiting): metricWaiting + `{model_name="` + testModelID + `",engine="0",x="y"} 0.0`,
		}), errMetricsUnexpected},
		"unknown finish reason": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			successLine("abort"): vllmSample(metricSuccessTotal, testModelID, "0", "cancelled", "0.0"),
		}), errMetricsUnexpected},
		"duplicate gauge": {vllmMetricsBody(testModelID, 0, 0, 0) + gaugeLine(metricRunning) + " 0.0\n", errMetricsDuplicate},
		"duplicate reason": {vllmMetricsBody(testModelID, 0, 0, 0) + successLine("error") + " 0.0\n", errMetricsDuplicate},
		"duplicate label key": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			gaugeLine(metricRunning): metricRunning + `{model_name="` + testModelID + `",engine="0",engine="0"} 0.0`,
		}), errMetricsMalformed},
		"timestamp":            {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " 0.0 1700000000000"}), errMetricsMalformed},
		"no value":             {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning)}), errMetricsMalformed},
		"unterminated labels":  {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): metricRunning + `{model_name="` + testModelID + `",engine="0" 0.0`}), errMetricsMalformed},
		"unknown escape":       {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): metricRunning + `{model_name="a\q",engine="0"} 0.0`}), errMetricsMalformed},
		"negative":             {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricWaiting): gaugeLine(metricWaiting) + " -1.0"}), errMetricsMalformed},
		"NaN":                  {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " NaN"}), errMetricsMalformed},
		"infinite":             {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{successLine("stop"): successLine("stop") + " +Inf"}), errMetricsMalformed},
		"hex":                  {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " 0x1"}), errMetricsMalformed},
		"fractional running":   {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " 0.5"}), errMetricsRange},
		"tiny running":         {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " 1e-300"}), errMetricsRange},
		"fractional exponent":  {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{successLine("stop"): successLine("stop") + " 1.2345678e+06"}), errMetricsRange},
		"count past exact":     {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{successLine("stop"): successLine("stop") + " 9007199254740993"}), errMetricsRange},
		"huge exponent":        {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{gaugeLine(metricWaiting): gaugeLine(metricWaiting) + " 1e+308"}), errMetricsRange},
		"success sum past exact": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			successLine("stop"):   successLine("stop") + " " + maxCount,
			successLine("length"): successLine("length") + " 1",
		}), errMetricsRange},
		"queue sum past exact": {editLines(vllmMetricsLines(testModelID, 0, 0, 0), map[string]string{
			gaugeLine(metricRunning): gaugeLine(metricRunning) + " " + maxCount,
			gaugeLine(metricWaiting): gaugeLine(metricWaiting) + " 1",
		}), errMetricsRange},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseVLLMMetrics([]byte(tc.body), testModelID)
			require.ErrorIs(t, err, tc.want)
			assert.NotContains(t, err.Error(), testModelID, "errors never quote the response")
		})
	}
}

// One finish reason missing while another absorbs its count leaves the sum
// unchanged; it is still not a complete sample.
func TestParseVLLMMetrics_MissingFinishReasonWithUnchangedTotal(t *testing.T) {
	lines := vllmMetricsLines(testModelID, 0, 0, 0)
	whole := editLines(lines, map[string]string{successLine("stop"): successLine("stop") + " 8.0", successLine("length"): successLine("length") + " 1.0"})
	m, err := parseVLLMMetrics([]byte(whole), testModelID)
	require.NoError(t, err)
	require.EqualValues(t, 9, m.successTotal)

	partial := editLines(lines, map[string]string{successLine("stop"): successLine("stop") + " 9.0", successLine("length"): ""})
	_, err = parseVLLMMetrics([]byte(partial), testModelID)
	require.ErrorIs(t, err, errMetricsIncomplete)
}

// vLLM's counters are floats: at 2^53 one more request may not change the
// value, so only counts below it are exact evidence of no new work.
func TestParseVLLMMetrics_ExactCountBoundary(t *testing.T) {
	lines := vllmMetricsLines(testModelID, 0, 0, 0)
	for value, want := range map[string]error{
		"9007199254740991":       nil,
		"9.007199254740991e+15":  nil,
		"9007199254740992":       errMetricsRange,
		"9.007199254740992e+15":  errMetricsRange,
	} {
		t.Run(value, func(t *testing.T) {
			body := editLines(lines, map[string]string{successLine("stop"): successLine("stop") + " " + value})
			m, err := parseVLLMMetrics([]byte(body), testModelID)
			if want != nil {
				require.ErrorIs(t, err, want)
				return
			}
			require.NoError(t, err)
			assert.EqualValues(t, uint64(1<<53-1), m.successTotal)
		})
	}
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

var probeMetricsBody = vllmMetricsBody(testModelID, 1, 2, 9)

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
	m, err := probeMetrics(t.Context(), srv.Client(), srv.URL, testModelID)
	require.NoError(t, err)
	assert.Equal(t, 3, m.inFlight())
	assert.EqualValues(t, 9, m.successTotal)
	assert.EqualValues(t, 1, hits.Load())
}

func TestProbeMetrics_OrdinaryFailuresAreNotRedirects(t *testing.T) {
	for _, status := range []int{http.StatusNotModified, http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			_, err := probeMetrics(t.Context(), srv.Client(), srv.URL, testModelID)
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

			_, err := probeMetrics(t.Context(), srv.Client(), srv.URL, testModelID)
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

			_, err := probeMetrics(t.Context(), srv.Client(), srv.URL, testModelID)
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

	_, err := probeMetrics(t.Context(), client, srv.URL, testModelID)
	require.ErrorIs(t, err, errMetricsRedirected, "an injected following policy is not used")
	assert.False(t, followed)
	assert.Zero(t, hits.Load())
	assert.EqualValues(t, 1, transport.calls.Load(), "the probe bypassed the injected transport")
	assert.Same(t, transport, client.Transport.(*countingTransport), "the probe replaced the injected transport")
	require.NotNil(t, client.CheckRedirect, "the probe mutated the injected client's redirect policy")

	m, err := probeMetrics(t.Context(), client, target.URL, testModelID)
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

	_, err = probeMetrics(t.Context(), srv.Client(), u.String(), testModelID)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), safetyEndpointPass)
	assert.NotContains(t, err.Error(), safetyEndpointPath)

	closed := "http://" + safetyEndpointUser + ":" + safetyEndpointPass + "@127.0.0.1:" + safetyClosedPort(t) + "/" + safetyEndpointPath
	_, err = probeMetrics(t.Context(), srv.Client(), closed, testModelID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errMetricsRedirected)
	assert.NotContains(t, err.Error(), safetyEndpointPass)
	assert.NotContains(t, err.Error(), safetyEndpointPath)
}

// paddedMetricsServer serves a complete scrape padded by an unrelated comment
// to size bytes, flushed first so the response declares no length.
func paddedMetricsServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	body := probeMetricsBody + "# "
	body += strings.Repeat("x", size-len(body)-1) + "\n"
	require.Len(t, body, size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body[:1024]))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(body[1024:]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A complete scrape exactly at the read limit is read; one byte more is not
// read as a sample, though everything before the limit is a valid scrape.
func TestProbeMetrics_ReadLimitBoundary(t *testing.T) {
	at := paddedMetricsServer(t, metricsBodyLimit)
	m, err := probeMetrics(t.Context(), at.Client(), at.URL, testModelID)
	require.NoError(t, err)
	assert.Equal(t, 3, m.inFlight())

	over := paddedMetricsServer(t, metricsBodyLimit+1)
	resp, err := over.Client().Get(over.URL)
	require.NoError(t, err)
	assert.EqualValues(t, -1, resp.ContentLength, "the oversized response must declare no length")
	require.NoError(t, resp.Body.Close())

	_, err = probeMetrics(t.Context(), over.Client(), over.URL, testModelID)
	require.ErrorIs(t, err, errMetricsOversized)
}
