//go:build e2e

package diskperf

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
)

// Regression bands. Wide enough to survive bare-metal jitter, tight enough to
// catch the 2x that matters.
const (
	failFraction = 0.25
	warnFraction = 0.10
)

// baselineJSON is embedded rather than read from disk: the suite is
// cross-compiled into a .test binary and shipped to the node without its
// source, so a path-relative read would find nothing there.
//
//go:embed baseline.json
var baselineJSON []byte

// Baseline is the committed reference the throughput and latency assertions
// compare against. It changes only by deliberate PR with a written
// justification -- a baseline that updates itself is not a gate.
type Baseline struct {
	Version int `json:"version"`
	// Origin records the hardware and build the numbers were taken on. A
	// figure with no stated origin cannot be judged for whether it still
	// applies to the cell running the gate.
	Origin string                 `json:"origin"`
	Jobs   map[string]BaselineJob `json:"jobs"`
}

// BaselineJob holds the reference metrics for one job. A zero field means "not
// measured": it records and warns rather than failing, so the gate can land and
// run before hardware numbers exist.
type BaselineJob struct {
	ReadIOPS    float64 `json:"read_iops"`
	WriteIOPS   float64 `json:"write_iops"`
	ReadP999Ms  float64 `json:"read_p99_9_ms"`
	WriteP999Ms float64 `json:"write_p99_9_ms"`
}

func loadBaseline() (Baseline, error) {
	var b Baseline
	if err := json.Unmarshal(baselineJSON, &b); err != nil {
		return Baseline{}, fmt.Errorf("parsing embedded baseline.json: %w", err)
	}
	return b, nil
}

// verdict is the outcome of comparing one metric against its reference.
type verdict int

const (
	verdictUnmeasured verdict = iota
	verdictOK
	verdictWarn
	verdictFail
)

// metric is one measured value and its reference.
type metric struct {
	Name string
	Got  float64
	Want float64
	// HigherIsBetter distinguishes throughput from latency, which regress in
	// opposite directions.
	HigherIsBetter bool
}

// regression returns the fractional move in the bad direction, negative when
// the measurement improved on the reference.
func (m metric) regression() float64 {
	if m.Want == 0 {
		return 0
	}
	delta := (m.Want - m.Got) / m.Want
	if !m.HigherIsBetter {
		delta = -delta
	}
	return delta
}

// judge classifies the metric against the bands. An unmeasured reference or a
// measurement fio did not produce is reported as such rather than compared,
// since comparing against zero manufactures either a pass or a failure from no
// evidence.
func (m metric) judge() (verdict, string) {
	if m.Want == 0 {
		return verdictUnmeasured, fmt.Sprintf("%s: %.1f (no committed baseline — recorded, not gated)", m.Name, m.Got)
	}
	if m.Got == 0 || math.IsNaN(m.Got) {
		return verdictUnmeasured, fmt.Sprintf("%s: not reported by fio (baseline %.1f)", m.Name, m.Want)
	}
	r := m.regression()
	msg := fmt.Sprintf("%s: %.1f vs baseline %.1f (%+.1f%%)", m.Name, m.Got, m.Want, -r*100)
	switch {
	case r > failFraction:
		return verdictFail, msg
	case r > warnFraction:
		return verdictWarn, msg
	default:
		return verdictOK, msg
	}
}

// metricsFor pairs a job's measured aggregate with its reference. Read metrics
// are omitted for a write-only job: fio reports zeros for the idle stream, and
// a zero read IOPS is not a regression there.
func metricsFor(job jobSpec, agg fioJob, base BaselineJob) []metric {
	var ms []metric
	if agg.Write.IOBytes > 0 {
		ms = append(ms,
			metric{Name: "write_iops", Got: agg.Write.IOPS, Want: base.WriteIOPS, HigherIsBetter: true},
			metric{Name: "write_p99_9_ms", Got: agg.Write.p999Ms(), Want: base.WriteP999Ms},
		)
	}
	if agg.Read.IOBytes > 0 {
		ms = append(ms,
			metric{Name: "read_iops", Got: agg.Read.IOPS, Want: base.ReadIOPS, HigherIsBetter: true},
			metric{Name: "read_p99_9_ms", Got: agg.Read.p999Ms(), Want: base.ReadP999Ms},
		)
	}
	return ms
}
