package ochre

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// vllmEngines is the engine set the guest launcher starts: no supported launch
// passes a data-parallel size, so vLLM runs engine 0 alone.
var vllmEngines = []string{"0"}

// vllmMetricsFinishReasons is the pinned vLLM's finish-reason set. It exports a
// success counter for every reason on every engine from startup.
var vllmMetricsFinishReasons = []string{"stop", "length", "abort", "error", "repetition"}

// maxExactCount bounds every count and sum, so each converts to the stored
// int and float64 fields without loss.
const maxExactCount = 1 << 53

// metricsBodyLimit caps how much of /metrics is read. vLLM's exposition is a
// few tens of KB; anything past this is a wedged or hostile endpoint, and the
// scrape runs on the daemon, so it must not be able to exhaust its memory.
const metricsBodyLimit = 4 << 20

var (
	errMetricsIdentity   = errors.New("primary vLLM member identity cannot be established")
	errMetricsOversized  = errors.New("metrics response exceeds the read limit")
	errMetricsMalformed  = errors.New("required metrics sample is malformed")
	errMetricsUnexpected = errors.New("required metrics sample has unexpected labels")
	errMetricsDuplicate  = errors.New("required metrics sample is duplicated")
	errMetricsIncomplete = errors.New("required metrics samples are missing")
	errMetricsRange      = errors.New("required metrics sample is not an exact whole count")
)

// endpointMetrics is one sample of a serving endpoint's load.
type endpointMetrics struct {
	running      uint64
	waiting      uint64
	successTotal uint64
}

// idle reports whether this sample, compared against the previous sweep's
// successTotal, means nothing is being served. All three conditions are
// required: an empty queue alone would call a request that started and
// finished entirely between two sweeps idle.
func (m endpointMetrics) idle(prevSuccessTotal float64) bool {
	return m.running == 0 && m.waiting == 0 && float64(m.successTotal) == prevSuccessTotal
}

// inFlight is the queue depth the scheduler reported, published so an
// eviction decision on another replica can tell a quiet endpoint from a busy
// one without scraping it itself.
func (m endpointMetrics) inFlight() int {
	return int(m.running + m.waiting)
}

// vllmModelFor is the served model name of rec's primary vLLM member. A record
// without exactly one such member cannot be checked and stays held.
func vllmModelFor(rec EndpointRecord) (string, error) {
	var model string
	for id, m := range rec.Members {
		if m.Family != FamilyMeta {
			continue
		}
		if model != "" || id == "" {
			return "", errMetricsIdentity
		}
		model = id
	}
	if model == "" {
		return "", errMetricsIdentity
	}
	return model, nil
}

// errMetricsRedirected is a metrics probe answered with a redirect, which is
// neither followed nor read as metrics.
var errMetricsRedirected = errors.New("bedrock: metrics probe was redirected")

// probeMetrics reads baseURL + "/metrics" without following a redirect and
// requires the complete sample for model. A redirect is errMetricsRedirected;
// errors name the endpoint by scheme and host, never a Location or the body.
func probeMetrics(ctx context.Context, client *http.Client, baseURL, model string) (endpointMetrics, error) {
	id := endpointLogID(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		return endpointMetrics{}, fmt.Errorf("bedrock: metrics probe %s: invalid request", id)
	}
	// The connection is not pooled, so unread bytes are never logged as idle.
	req.Close = true
	probe, recorder := noFollow(client)
	resp, err := probe.Do(req)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	switch {
	case recorder.redirected:
		return endpointMetrics{}, fmt.Errorf("%w: %s", errMetricsRedirected, id)
	case err != nil:
		return endpointMetrics{}, fmt.Errorf("bedrock: metrics probe %s: %s", id, transportFailureClass(err))
	case resp.StatusCode != http.StatusOK:
		return endpointMetrics{}, fmt.Errorf("bedrock: metrics probe %s: HTTP %d", id, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, metricsBodyLimit+1))
	switch {
	case err != nil:
		return endpointMetrics{}, fmt.Errorf("bedrock: metrics probe %s: incomplete response", id)
	case len(body) > metricsBodyLimit:
		return endpointMetrics{}, fmt.Errorf("bedrock: metrics probe %s: %w", id, errMetricsOversized)
	}
	m, err := parseVLLMMetrics(body, model)
	if err != nil {
		return endpointMetrics{}, fmt.Errorf("bedrock: metrics probe %s: %w", id, err)
	}
	return m, nil
}

// parseVLLMMetrics requires exactly one well-formed sample per expected engine
// of each gauge, and of the success counter per finish reason, all for model.
// Other series are ignored; errors never quote the response.
func parseVLLMMetrics(body []byte, model string) (endpointMetrics, error) {
	var m endpointMetrics
	seen := make(map[string]bool)
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		text := strings.TrimSuffix(string(line), "\r")
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name := text
		if i := strings.IndexAny(text, "{ \t"); i >= 0 {
			name = text[:i]
		}
		var sum *uint64
		labelKeys := []string{"engine", "model_name"}
		switch name {
		case metricRunning:
			sum = &m.running
		case metricWaiting:
			sum = &m.waiting
		case metricSuccessTotal:
			sum = &m.successTotal
			labelKeys = []string{"engine", "finished_reason", "model_name"}
		default:
			continue
		}
		labels, raw, err := splitRequiredSample(text[len(name):])
		if err != nil {
			return endpointMetrics{}, err
		}
		if !slices.Equal(sortedKeys(labels), labelKeys) || labels["model_name"] != model ||
			!slices.Contains(vllmEngines, labels["engine"]) ||
			(name == metricSuccessTotal && !slices.Contains(vllmMetricsFinishReasons, labels["finished_reason"])) {
			return endpointMetrics{}, errMetricsUnexpected
		}
		key := name + "\x00" + labels["engine"] + "\x00" + labels["finished_reason"]
		if seen[key] {
			return endpointMetrics{}, errMetricsDuplicate
		}
		seen[key] = true
		value, err := parseWholeCount(raw)
		if err != nil {
			return endpointMetrics{}, err
		}
		if *sum += value; *sum > maxExactCount {
			return endpointMetrics{}, errMetricsRange
		}
	}
	if len(seen) != len(vllmEngines)*(2+len(vllmMetricsFinishReasons)) {
		return endpointMetrics{}, errMetricsIncomplete
	}
	if m.running+m.waiting > maxExactCount {
		return endpointMetrics{}, errMetricsRange
	}
	return m, nil
}

// splitRequiredSample parses the label set and the single value that follow a
// required series name. A trailing timestamp or any other field is malformed.
func splitRequiredSample(rest string) (map[string]string, string, error) {
	labels := map[string]string{}
	if strings.HasPrefix(rest, "{") {
		var err error
		labels, rest, err = parseLabels(rest[1:])
		if err != nil {
			return nil, "", err
		}
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 || !strings.HasPrefix(rest, " ") {
		return nil, "", errMetricsMalformed
	}
	return labels, fields[0], nil
}

// parseLabels reads `key="value",...}` and returns the labels and what follows
// the closing brace. Values may use the exposition escapes \\, \" and \n.
func parseLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for !strings.HasPrefix(s, "}") {
		eq := strings.Index(s, `="`)
		if eq <= 0 {
			return nil, "", errMetricsMalformed
		}
		key := s[:eq]
		if _, dup := labels[key]; dup || strings.ContainsAny(key, " \t,{}=\"\\") {
			return nil, "", errMetricsMalformed
		}
		s = s[eq+2:]
		var value strings.Builder
		i := 0
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] != '\\' {
				value.WriteByte(s[i])
				continue
			}
			if i++; i == len(s) {
				return nil, "", errMetricsMalformed
			}
			switch s[i] {
			case '\\', '"':
				value.WriteByte(s[i])
			case 'n':
				value.WriteByte('\n')
			default:
				return nil, "", errMetricsMalformed
			}
		}
		if i == len(s) {
			return nil, "", errMetricsMalformed
		}
		labels[key] = value.String()
		s = s[i+1:]
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case !strings.HasPrefix(s, "}"):
			return nil, "", errMetricsMalformed
		}
	}
	return labels, s[1:], nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// parseWholeCount reads a sample value exactly, as digits, a decimal or the
// exponent form Prometheus clients print, and accepts only a whole count no
// greater than maxExactCount. Nothing is rounded.
func parseWholeCount(raw string) (uint64, error) {
	mantissa, exponent := raw, 0
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		mantissa = raw[:i]
		exp, sign := raw[i+1:], 1
		switch {
		case strings.HasPrefix(exp, "+"):
			exp = exp[1:]
		case strings.HasPrefix(exp, "-"):
			exp, sign = exp[1:], -1
		}
		if exp == "" || len(exp) > 3 || !allDigits(exp) {
			return 0, errMetricsMalformed
		}
		n, _ := strconv.Atoi(exp)
		exponent = sign * n
	}
	whole, frac, _ := strings.Cut(mantissa, ".")
	if whole == "" || !allDigits(whole) || !allDigits(frac) {
		return 0, errMetricsMalformed
	}
	digits := strings.TrimLeft(whole+frac, "0")
	if shift := exponent - len(frac); shift < 0 {
		cut := max(len(digits)+shift, 0)
		if strings.Trim(digits[cut:], "0") != "" {
			return 0, errMetricsRange
		}
		digits = digits[:cut]
	} else if digits != "" {
		if len(digits)+shift > 16 {
			return 0, errMetricsRange
		}
		digits += strings.Repeat("0", shift)
	}
	if digits == "" {
		return 0, nil
	}
	if len(digits) > 16 {
		return 0, errMetricsRange
	}
	v, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || v > maxExactCount {
		return 0, errMetricsRange
	}
	return v, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
