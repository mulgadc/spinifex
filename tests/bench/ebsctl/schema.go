// Package ebsctl benchmarks EC2 control-plane volume-operation latency
// against a live Spinifex cluster under both [ebs] provider settings
// (embedded vs viperblockd), isolating API round-trip latency from
// time-to-desired-state settle time and tallying the error surface each
// provider produces.
//
// This file and stats.go/collector.go/compare.go carry no build tag: they
// hold the JSON schema and pure statistics/comparison math, so they compile
// and unit-test without a cluster. The cluster-driving code lives in files
// tagged `e2e bench` (both required, since it imports the e2e harness
// package, which is itself gated on `e2e`).
package ebsctl

import "time"

// RunMeta is the run-level metadata recorded alongside the per-operation
// results, so a result file is self-describing when read back later without
// the invocation that produced it.
type RunMeta struct {
	Timestamp time.Time `json:"timestamp"`
	// GitSHA is the build/runtime git commit, best-effort (see gitSHA in
	// main_test.go). "unknown" when neither the runtime repo lookup nor an
	// -ldflags override was available.
	GitSHA string `json:"git_sha"`
	// Provider is the [ebs] provider value determined to be in effect on the
	// cluster for this run: "embedded" or "viperblockd".
	Provider string `json:"ebs_provider"`
	// ProviderSource records how Provider was determined: "cluster-local-config",
	// "cluster-ssh", or "flag" (operator-supplied, cluster detection failed).
	ProviderSource       string `json:"ebs_provider_source"`
	NodeCount            int    `json:"node_count"`
	IterationsPerWorker  int    `json:"iterations_per_worker"`
	WarmupPerWorker      int    `json:"warmup_per_worker"`
	Concurrency          int    `json:"concurrency"`
	AttachDetachIncluded bool   `json:"attach_detach_included"`
	VolumeSizeGiB        int64  `json:"volume_size_gib"`
}

// MinSamplesForP95 is the sample-count heuristic below which a reported p95
// is a near-single-point estimate rather than a meaningful percentile (at
// n=20, p95 sits at rank ~19, i.e. only the top sample or two beyond it).
// It is not a hard statistical threshold — SeriesStats.P95Reliable just
// makes the honesty call visible in the output rather than silent.
const MinSamplesForP95 = 20

// SeriesStats summarises one latency sample series in milliseconds, and
// carries the raw samples so a later comparison can be redone without
// re-running the benchmark. p99 is deliberately not reported anywhere in
// this schema: at the default sample sizes here (tens of samples) p99 is
// one data point dressed up as a percentile.
type SeriesStats struct {
	Count int `json:"count"`
	// Median/P95/Mean/Min/Max are all in milliseconds.
	Median float64 `json:"median_ms"`
	P95    float64 `json:"p95_ms"`
	// P95Reliable is false when Count < MinSamplesForP95 — a signal to the
	// reader that P95 above is indicative only.
	P95Reliable bool      `json:"p95_reliable"`
	Mean        float64   `json:"mean_ms"`
	Min         float64   `json:"min_ms"`
	Max         float64   `json:"max_ms"`
	Samples     []float64 `json:"samples_ms"`
}

// ErrorTally counts operation errors by AWS error code and by message, for
// the same operation over the same run.
type ErrorTally struct {
	Total     int            `json:"total"`
	ByCode    map[string]int `json:"by_code,omitempty"`
	ByMessage map[string]int `json:"by_message,omitempty"`
}

func newErrorTally() *ErrorTally {
	return &ErrorTally{ByCode: map[string]int{}, ByMessage: map[string]int{}}
}

// add records one error occurrence. Empty code/message are folded into
// "unknown" so the tally maps never grow an empty-string key.
func (e *ErrorTally) add(code, message string) {
	e.Total++
	if code == "" {
		code = "unknown"
	}
	if message == "" {
		message = "unknown"
	}
	e.ByCode[code]++
	e.ByMessage[message]++
}

// rate returns Total as a fraction of attempts, or 0 when attempts is 0.
func (e *ErrorTally) rate(attempts int) float64 {
	if attempts == 0 {
		return 0
	}
	return float64(e.Total) / float64(attempts)
}

// OpResult is the full measured result for one operation kind (e.g.
// "CreateVolume", "DescribeVolumes.Single").
type OpResult struct {
	Operation string `json:"operation"`
	// Attempts is every call made (successes + errors), across every worker,
	// excluding discarded warm-up iterations.
	Attempts   int          `json:"attempts"`
	APILatency SeriesStats  `json:"api_latency"`
	SettleTime *SeriesStats `json:"settle_time,omitempty"`
	// SettleTimeouts counts successful API calls whose settle-state poll
	// exceeded its budget — distinct from Errors, which is only API-call
	// failures. A settle timeout means the call succeeded but the resource
	// never (within budget) reached the expected state.
	SettleTimeouts int        `json:"settle_timeouts"`
	Errors         ErrorTally `json:"errors"`
	ErrorRate      float64    `json:"error_rate"`
}

// RunResult is the top-level JSON document the benchmark writes and the
// comparison tool reads.
type RunResult struct {
	Meta       RunMeta              `json:"meta"`
	Operations map[string]*OpResult `json:"operations"`
}
