// Package loadgen drives a measured request load at the control plane and
// reports per-operation latency distributions.
//
// It exists because bash cannot measure this: a shell driving the AWS CLI pays
// over a second of interpreter start-up per call, which is larger than the
// latency being measured, so a percentile built from those samples is a
// percentile of the CLI. The engine here holds long-lived SDK clients and times
// only the request.
package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Mode is how a stage paces its requests, and the two answer different
// questions. Closed loop asks "with N callers in flight, how fast is it?" and
// slows down when the server does. Open loop asks "at R requests a second, does
// it keep up?" and does not, which is the only way to see a queue form.
type Mode string

const (
	ModeClosed Mode = "closed"
	ModeOpen   Mode = "open"
)

// Kind separates reads from writes because their SLOs differ by an order of
// magnitude: a Describe that takes two seconds is broken, a RunInstances that
// takes two seconds is normal.
type Kind string

const (
	KindRead  Kind = "read"
	KindWrite Kind = "write"
)

// ErrShed marks a request an open-loop stage could not issue because too many
// were already in flight. It is recorded rather than dropped: a stage that
// cannot sustain its rate has found the answer it was looking for.
var ErrShed = errors.New("shed: too many requests in flight")

// shedCode is the error code shed requests are counted under. It is namespaced
// so it can never collide with a code the service returns.
const shedCode = "loadgen.Shed"

// Op is one control-plane call under test. Call does the request and returns
// the service's error unchanged, so the code can be classified.
type Op struct {
	Name string
	Kind Kind
	Call func(ctx context.Context, target *Target) error
}

// Target is one tenant's wiring. A run spreads its workers across every target
// it is given, so the load looks like several customers rather than one.
type Target struct {
	Account string
	Profile string
	Clients *Clients
	// VPCID is resolved once before the run for operations that need a
	// resource of the tenant's own to act on. Resolving it per request would
	// put a second call inside every sample.
	VPCID string
	// VolumeID lets a run ask for one volume by id rather than for the whole
	// listing. The difference between the two is the cost of the listing,
	// which is otherwise indistinguishable from the cost of the request.
	VolumeID string
}

// Stage is one step of a ramp. Exactly one of Concurrency or RPS applies,
// depending on Mode.
type Stage struct {
	Mode        Mode          `json:"mode"`
	Concurrency int           `json:"concurrency,omitempty"`
	RPS         float64       `json:"rps,omitempty"`
	Duration    time.Duration `json:"-"`
	DurationSec float64       `json:"durationSeconds"`
	// MaxInFlight caps how many open-loop requests may be outstanding at once.
	// Zero means four seconds' worth of the stage's rate, which is enough slack
	// for a slow cluster and not enough for a stalled one to exhaust memory.
	MaxInFlight int `json:"maxInFlight,omitempty"`
}

// SLO is the pass/fail line. A stage breaches when an operation's p99 exceeds
// the limit for its kind, or when any request fails for a reason other than a
// deliberately tripped quota.
type SLO struct {
	ReadP99  time.Duration `json:"-"`
	WriteP99 time.Duration `json:"-"`
	// ExpectedErrorCodes are not failures. A quota rejection means the cluster
	// is working; counting it as an error would fail a correct run.
	ExpectedErrorCodes []string `json:"expectedErrorCodes,omitempty"`
	ReadP99MS          float64  `json:"readP99Ms"`
	WriteP99MS         float64  `json:"writeP99Ms"`
}

// OpStats is one operation's result within one stage.
type OpStats struct {
	Op         string         `json:"op"`
	Kind       Kind           `json:"kind"`
	Count      int            `json:"count"`
	Errors     int            `json:"errors"`
	ErrorCodes map[string]int `json:"errorCodes,omitempty"`
	MeanMS     float64        `json:"meanMs"`
	P50MS      float64        `json:"p50Ms"`
	P90MS      float64        `json:"p90Ms"`
	P99MS      float64        `json:"p99Ms"`
	MaxMS      float64        `json:"maxMs"`
	// AchievedRPS is what the stage actually managed, which is the number that
	// matters in open loop: asking for 40 and achieving 12 is the finding.
	AchievedRPS float64 `json:"achievedRps"`
}

// StageResult is one stage's outcome.
type StageResult struct {
	Stage    Stage              `json:"stage"`
	Ops      map[string]OpStats `json:"ops"`
	Started  time.Time          `json:"started"`
	Ended    time.Time          `json:"ended"`
	Breached []string           `json:"breached,omitempty"`
}

// Report is the whole run. It is written as JSON for the harness to fold into
// its run directory, and summarised for a human.
type Report struct {
	Endpoint string        `json:"endpoint"`
	Region   string        `json:"region"`
	Targets  []string      `json:"targets"`
	SLO      SLO           `json:"slo"`
	Started  time.Time     `json:"started"`
	Ended    time.Time     `json:"ended"`
	Stages   []StageResult `json:"stages"`
	// FirstBreach names the first stage of each mode whose p99 crossed the SLO.
	// This is the capacity number the run exists to produce.
	FirstBreach map[Mode]*Breach `json:"firstBreach,omitempty"`
}

// Breach records where a mode stopped meeting its SLO.
type Breach struct {
	StageIndex  int      `json:"stageIndex"`
	Concurrency int      `json:"concurrency,omitempty"`
	RPS         float64  `json:"rps,omitempty"`
	Ops         []string `json:"ops"`
}

// sample is one timed request.
type sample struct {
	op      string
	latency time.Duration
	errCode string
	failed  bool
}

// Run executes every stage in order and returns the report. Stages run back to
// back with no cool-down: a queue left by one stage is a real condition the
// next one should see, and hiding it would flatter the result.
func Run(ctx context.Context, targets []*Target, ops []Op, stages []Stage, slo SLO) (*Report, error) {
	if len(targets) == 0 {
		return nil, errors.New("loadgen: no targets")
	}
	if len(ops) == 0 {
		return nil, errors.New("loadgen: no operations")
	}

	report := &Report{
		SLO: slo, Started: time.Now().UTC(), FirstBreach: map[Mode]*Breach{},
	}
	for _, target := range targets {
		report.Targets = append(report.Targets, target.Account)
	}

	for index, stage := range stages {
		// An interrupted run reports the stages that finished rather than
		// nothing: someone who stops a ramp still wants the rungs it climbed.
		select {
		case <-ctx.Done():
			report.Ended = time.Now().UTC()
			return report, nil
		default:
		}

		result := runStage(ctx, targets, ops, stage)
		result.Breached = breachedOps(result, slo)
		report.Stages = append(report.Stages, result)

		if len(result.Breached) > 0 && report.FirstBreach[stage.Mode] == nil {
			report.FirstBreach[stage.Mode] = &Breach{
				StageIndex: index, Concurrency: stage.Concurrency,
				RPS: stage.RPS, Ops: result.Breached,
			}
		}
	}

	report.Ended = time.Now().UTC()
	return report, nil
}

func runStage(ctx context.Context, targets []*Target, ops []Op, stage Stage) StageResult {
	stage.DurationSec = stage.Duration.Seconds()
	started := time.Now()
	var samples []sample
	switch stage.Mode {
	case ModeOpen:
		samples = runOpen(ctx, targets, ops, stage)
	case ModeClosed:
		fallthrough
	default:
		stage.Mode = ModeClosed
		samples = runClosed(ctx, targets, ops, stage)
	}
	ended := time.Now()

	return StageResult{
		Stage:   stage,
		Ops:     summarise(samples, ops, ended.Sub(started)),
		Started: started.UTC(),
		Ended:   ended.UTC(),
	}
}

// runClosed holds a fixed number of callers in flight. Each worker walks the
// operation list so every operation gets the same share of the concurrency,
// rather than whichever one happens to be fastest getting the most turns.
func runClosed(ctx context.Context, targets []*Target, ops []Op, stage Stage) []sample {
	concurrency := max(stage.Concurrency, 1)
	stageCtx, cancel := context.WithTimeout(ctx, stage.Duration)
	defer cancel()

	collected := make([][]sample, concurrency)
	var wg sync.WaitGroup
	for worker := range concurrency {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			target := targets[worker%len(targets)]
			for turn := 0; stageCtx.Err() == nil; turn++ {
				op := ops[(worker+turn)%len(ops)]
				if result, ok := timeCall(stageCtx, target, op, time.Now()); ok {
					collected[worker] = append(collected[worker], result)
				}
			}
		}(worker)
	}
	wg.Wait()

	var samples []sample
	for _, worker := range collected {
		samples = append(samples, worker...)
	}
	return samples
}

// runOpen issues requests on a fixed schedule regardless of how the server is
// coping, and times each one from when it was *due* rather than when it
// started. Timing from the start instead would hide the queue: a request held
// up for a second before it was issued would report as fast.
func runOpen(ctx context.Context, targets []*Target, ops []Op, stage Stage) []sample {
	rps := stage.RPS
	if rps <= 0 {
		rps = 1
	}
	interval := time.Duration(float64(time.Second) / rps)
	stageCtx, cancel := context.WithTimeout(ctx, stage.Duration)
	defer cancel()

	// In-flight requests are capped so a stalled cluster cannot make the
	// generator allocate without bound. Requests over the cap are recorded as
	// shed, which is the honest way to say the rate was not sustained.
	maxInFlight := stage.MaxInFlight
	if maxInFlight < 1 {
		maxInFlight = int(math.Max(rps*maxInFlightSeconds, minInFlight))
	}
	inFlight := make(chan struct{}, maxInFlight)

	var mu sync.Mutex
	var samples []sample
	var wg sync.WaitGroup

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for tick := 0; ; tick++ {
		select {
		case <-stageCtx.Done():
			wg.Wait()
			return samples
		case due := <-ticker.C:
			target := targets[tick%len(targets)]
			op := ops[tick%len(ops)]

			select {
			case inFlight <- struct{}{}:
			default:
				mu.Lock()
				samples = append(samples, sample{op: op.Name, errCode: shedCode, failed: true})
				mu.Unlock()
				continue
			}

			wg.Go(func() {
				defer func() { <-inFlight }()
				result, ok := timeCall(stageCtx, target, op, due)
				if !ok {
					return
				}
				mu.Lock()
				samples = append(samples, result)
				mu.Unlock()
			})
		}
	}
}

const (
	// How many seconds of requests may be in flight at once in open loop.
	maxInFlightSeconds = 4.0
	minInFlight        = 8.0
)

// timeCall runs one operation and measures from due, which is the time the
// request was scheduled for. In closed loop that is the moment the call
// starts; in open loop it is the tick, so queueing counts against the result.
//
// It reports false for a request the stage's own deadline cut short. Those are
// not the cluster's failures — every worker has one in flight when time is up,
// so counting them would put a fixed error per worker into every stage and a
// truncated latency into every distribution.
func timeCall(ctx context.Context, target *Target, op Op, due time.Time) (sample, bool) {
	err := op.Call(ctx, target)
	if err != nil && ctx.Err() != nil && cancelledByStage(err) {
		return sample{}, false
	}

	result := sample{op: op.Name, latency: time.Since(due)}
	if err != nil {
		result.failed = true
		result.errCode = ErrorCode(err)
	}
	return result, true
}

// cancelledByStage reports whether an error is the shape a cancelled request
// takes. The caller checks the stage context separately, so a per-request
// timeout against a stalled cluster is still counted as the failure it is.
func cancelledByStage(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ErrorCode(err) == cancelledCode
}

// The code aws-sdk-go gives a request cancelled through its context.
const cancelledCode = "RequestCanceled"

func summarise(samples []sample, ops []Op, elapsed time.Duration) map[string]OpStats {
	kinds := make(map[string]Kind, len(ops))
	for _, op := range ops {
		kinds[op.Name] = op.Kind
	}

	latencies := map[string][]float64{}
	stats := map[string]OpStats{}
	for _, s := range samples {
		entry, ok := stats[s.op]
		if !ok {
			entry = OpStats{Op: s.op, Kind: kinds[s.op], ErrorCodes: map[string]int{}}
		}
		entry.Count++
		if s.failed {
			entry.Errors++
			entry.ErrorCodes[s.errCode]++
		}
		stats[s.op] = entry
		// A shed request has no latency to report: it was never issued, and
		// folding a zero into the distribution would improve the percentiles
		// of a stage that failed to keep up.
		if s.errCode != shedCode {
			latencies[s.op] = append(latencies[s.op], float64(s.latency)/float64(time.Millisecond))
		}
	}

	for name, entry := range stats {
		values := latencies[name]
		sort.Float64s(values)
		entry.MeanMS = round2(mean(values))
		entry.P50MS = round2(percentile(values, 0.50))
		entry.P90MS = round2(percentile(values, 0.90))
		entry.P99MS = round2(percentile(values, 0.99))
		if len(values) > 0 {
			entry.MaxMS = round2(values[len(values)-1])
		}
		if elapsed > 0 {
			entry.AchievedRPS = round2(float64(entry.Count) / elapsed.Seconds())
		}
		if len(entry.ErrorCodes) == 0 {
			entry.ErrorCodes = nil
		}
		stats[name] = entry
	}
	return stats
}

// percentile is nearest-rank over sorted values: the smallest sample at or
// above the quantile. No interpolation, so every reported number is a request
// that actually happened.
func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := min(max(int(math.Ceil(quantile*float64(len(sorted)))), 1), len(sorted))
	return sorted[rank-1]
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, v := range values {
		total += v
	}
	return total / float64(len(values))
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}

// breachedOps names every operation in a stage that missed the SLO, either on
// p99 or by failing at all. An empty result means the stage passed.
func breachedOps(result StageResult, slo SLO) []string {
	expected := make(map[string]bool, len(slo.ExpectedErrorCodes))
	for _, code := range slo.ExpectedErrorCodes {
		expected[code] = true
	}

	var breached []string
	for name, stats := range result.Ops {
		limit := slo.ReadP99
		if stats.Kind == KindWrite {
			limit = slo.WriteP99
		}
		if limit > 0 && stats.P99MS > float64(limit)/float64(time.Millisecond) {
			breached = append(breached, name)
			continue
		}
		for code, count := range stats.ErrorCodes {
			if count > 0 && !expected[code] {
				breached = append(breached, name)
				break
			}
		}
	}
	sort.Strings(breached)
	return breached
}

// Summary renders the report the way an operator reads it: one line per
// operation per stage, then the capacity number.
func Summary(report *Report) string {
	out := fmt.Sprintf("endpoint %s  region %s  tenants %d\n",
		report.Endpoint, report.Region, len(report.Targets))
	for index, stage := range report.Stages {
		out += fmt.Sprintf("\n[%d] %s ", index, stage.Stage.Mode)
		if stage.Stage.Mode == ModeOpen {
			out += fmt.Sprintf("%.0f rps", stage.Stage.RPS)
		} else {
			out += fmt.Sprintf("concurrency %d", stage.Stage.Concurrency)
		}
		out += fmt.Sprintf(" for %.0fs\n", stage.Stage.DurationSec)

		for _, name := range sortedOps(stage.Ops) {
			s := stage.Ops[name]
			out += fmt.Sprintf("    %-28s n=%-6d err=%-4d rps=%-7.2f p50=%-8.1f p90=%-8.1f p99=%-8.1f max=%.1f\n",
				name, s.Count, s.Errors, s.AchievedRPS, s.P50MS, s.P90MS, s.P99MS, s.MaxMS)
		}
		if len(stage.Breached) > 0 {
			out += fmt.Sprintf("    breached: %v\n", stage.Breached)
		}
	}

	out += "\n"
	for _, mode := range []Mode{ModeClosed, ModeOpen} {
		breach, ok := report.FirstBreach[mode]
		if !ok || breach == nil {
			out += fmt.Sprintf("%s: no breach at any stage run\n", mode)
			continue
		}
		if mode == ModeOpen {
			out += fmt.Sprintf("open: first breach at %.0f rps (stage %d) on %v\n",
				breach.RPS, breach.StageIndex, breach.Ops)
			continue
		}
		out += fmt.Sprintf("closed: first breach at concurrency %d (stage %d) on %v\n",
			breach.Concurrency, breach.StageIndex, breach.Ops)
	}
	return out
}

func sortedOps(ops map[string]OpStats) []string {
	names := make([]string, 0, len(ops))
	for name := range ops {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
