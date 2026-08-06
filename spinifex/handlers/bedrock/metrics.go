package handlers_bedrock

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The three vLLM series idleness is derived from. The first two are gauges of
// the scheduler's queues; the third is the monotonic count of completed
// requests, which is what distinguishes "nothing running right now" from
// "nothing has run since the last sweep".
const (
	metricRunning      = "vllm:num_requests_running"
	metricWaiting      = "vllm:num_requests_waiting"
	metricSuccessTotal = "vllm:request_success_total"
)

// metricsBodyLimit caps how much of /metrics is read. vLLM's exposition is a
// few tens of KB; anything past this is a wedged or hostile endpoint, and the
// scrape runs on the daemon, so it must not be able to exhaust its memory.
const metricsBodyLimit = 4 << 20

// endpointMetrics is one sample of a serving endpoint's load.
type endpointMetrics struct {
	running      float64
	waiting      float64
	successTotal float64
}

// idle reports whether this sample, compared against the previous sweep's
// successTotal, means nothing is being served. All three conditions are
// required: an empty queue alone would call a request that started and
// finished entirely between two sweeps idle.
func (m endpointMetrics) idle(prevSuccessTotal float64) bool {
	return m.running == 0 && m.waiting == 0 && m.successTotal == prevSuccessTotal
}

// inFlight is the queue depth the scheduler reported, published so an
// eviction decision on another replica can tell a quiet endpoint from a busy
// one without scraping it itself.
func (m endpointMetrics) inFlight() int {
	return int(m.running + m.waiting)
}

// scrapeMetrics reads baseURL + "/metrics" and extracts the three series
// idleness needs. Any failure is the caller's cue to treat the sample as
// unknown — never as idle, since a wedged VM must not be mistaken for a quiet
// one.
func scrapeMetrics(ctx context.Context, client *http.Client, baseURL string) (endpointMetrics, error) {
	url := baseURL + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return endpointMetrics{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return endpointMetrics{}, fmt.Errorf("bedrock: scrape %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return endpointMetrics{}, fmt.Errorf("bedrock: scrape %s: HTTP %d", url, resp.StatusCode)
	}
	m, err := parseVLLMMetrics(io.LimitReader(resp.Body, metricsBodyLimit))
	if err != nil {
		return endpointMetrics{}, fmt.Errorf("bedrock: scrape %s: %w", url, err)
	}
	return m, nil
}

// parseVLLMMetrics reads the Prometheus text exposition format and sums the
// three series of interest. Summing rather than taking the last value matters
// because each series is labelled per served model, and a server hosting more
// than one emits a line for each: the endpoint is only idle when every one of
// them is.
//
// A full Prometheus parser is deliberately not pulled in for three counters
// whose format is stable and documented.
func parseVLLMMetrics(r io.Reader) (endpointMetrics, error) {
	var m endpointMetrics
	var seen bool

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		switch name {
		case metricRunning:
			m.running += value
		case metricWaiting:
			m.waiting += value
		case metricSuccessTotal:
			m.successTotal += value
		default:
			continue
		}
		seen = true
	}
	if err := scanner.Err(); err != nil {
		return endpointMetrics{}, fmt.Errorf("read metrics: %w", err)
	}
	if !seen {
		// A 200 carrying none of the three series is not an idle endpoint: it
		// is something other than the vLLM server we expected to be there.
		return endpointMetrics{}, fmt.Errorf("no %s series in response", metricRunning)
	}
	return m, nil
}

// parseMetricLine splits one exposition line into its series name (labels
// stripped) and value. Returns ok=false for anything unparseable, which the
// caller skips: one malformed line must not discard a whole scrape.
func parseMetricLine(line string) (string, float64, bool) {
	// Sample lines carry an optional trailing timestamp, so the value is the
	// field after the name rather than the last field on the line.
	sep := strings.IndexAny(line, "{ ")
	if sep <= 0 {
		return "", 0, false
	}
	name := line[:sep]
	rest := line[sep:]
	if strings.HasPrefix(rest, "{") {
		end := strings.LastIndex(rest, "}")
		if end < 0 {
			return "", 0, false
		}
		rest = rest[end+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", 0, false
	}
	return name, value, true
}
