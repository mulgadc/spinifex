package ebsctl

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LoadResult reads and parses a RunResult JSON file written by the
// benchmark.
func LoadResult(path string) (*RunResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var r RunResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &r, nil
}

// bootstrapIters is the resample count for the percentile-bootstrap CI on
// the median difference. 2000 is a conventional floor for a stable 95% CI
// without being slow even for the largest sample vectors this tool sees.
const bootstrapIters = 2000

// resample draws len(s) values from s with replacement.
func resample(s []float64, rng *rand.Rand) []float64 {
	out := make([]float64, len(s))
	for i := range out {
		out[i] = s[rng.IntN(len(s))]
	}
	return out
}

// bootstrapMedianDiffCI returns a 95% percentile-bootstrap confidence
// interval for median(b) - median(a). Returns ok=false when either sample is
// empty (no meaningful CI to compute).
func bootstrapMedianDiffCI(a, b []float64, rng *rand.Rand) (lo, hi float64, ok bool) {
	if len(a) == 0 || len(b) == 0 {
		return 0, 0, false
	}
	diffs := make([]float64, bootstrapIters)
	for i := range bootstrapIters {
		aSorted := resample(a, rng)
		bSorted := resample(b, rng)
		sort.Float64s(aSorted)
		sort.Float64s(bSorted)
		diffs[i] = median(bSorted) - median(aSorted)
	}
	sort.Float64s(diffs)
	loIdx := int(0.025 * float64(bootstrapIters))
	hiIdx := int(0.975 * float64(bootstrapIters))
	if hiIdx >= bootstrapIters {
		hiIdx = bootstrapIters - 1
	}
	return diffs[loIdx], diffs[hiIdx], true
}

// unionOps returns the sorted union of operation names present in a and/or
// b, so the comparison table covers every operation either run measured
// (e.g. one run with attach/detach included, one without).
func unionOps(a, b *RunResult) []string {
	seen := map[string]struct{}{}
	for name := range a.Operations {
		seen[name] = struct{}{}
	}
	for name := range b.Operations {
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// seriesFor picks the api or settle series from op, or nil if op is nil or
// the requested series doesn't exist (settle time isn't tracked for every
// operation, e.g. DescribeVolumes).
func seriesFor(op *OpResult, metric string) *SeriesStats {
	if op == nil {
		return nil
	}
	if metric == "settle" {
		return op.SettleTime
	}
	return &op.APILatency
}

// CompareRuns renders a markdown comparison of a vs b: one table row per
// (operation, metric) pair present in either run, with medians, p95s,
// absolute/percentage delta, sample counts, error rates, and a bootstrap CI
// on the median difference as a crude, honest significance signal. seed
// makes the bootstrap resampling reproducible for tests; pass
// time.Now().UnixNano() for real use.
func CompareRuns(a, b *RunResult, seed int64) string {
	rng := rand.New(rand.NewPCG(0, uint64(seed))) //nolint:gosec // deterministic bootstrap resampling, not security-sensitive
	var sb strings.Builder

	fmt.Fprintf(&sb, "# ebsctl-bench comparison\n\n")
	fmt.Fprintf(&sb, "- **A**: provider=%s source=%s iterations/worker=%d concurrency=%d timestamp=%s sha=%s\n",
		a.Meta.Provider, a.Meta.ProviderSource, a.Meta.IterationsPerWorker, a.Meta.Concurrency,
		a.Meta.Timestamp.Format(time.RFC3339), a.Meta.GitSHA)
	fmt.Fprintf(&sb, "- **B**: provider=%s source=%s iterations/worker=%d concurrency=%d timestamp=%s sha=%s\n\n",
		b.Meta.Provider, b.Meta.ProviderSource, b.Meta.IterationsPerWorker, b.Meta.Concurrency,
		b.Meta.Timestamp.Format(time.RFC3339), b.Meta.GitSHA)

	fmt.Fprintf(&sb, "Deltas are indicative only unless the 95%% bootstrap CI on the median "+
		"difference excludes zero (Signal column). With modest sample counts in a shared, "+
		"noisy environment, a small median difference is not necessarily real — treat any row "+
		"whose CI straddles zero as noise until re-run with more samples.\n\n")

	fmt.Fprintf(&sb, "| Operation | Metric | A median (ms) | B median (ms) | Δ median (ms) | Δ%% | A p95 (ms) | B p95 (ms) | A n | B n | A err%% | B err%% | 95%% CI on Δ median (ms) | Signal |\n")
	fmt.Fprintf(&sb, "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|\n")

	for _, name := range unionOps(a, b) {
		opA, okA := a.Operations[name]
		opB, okB := b.Operations[name]
		writeRow(&sb, rng, name, "api", opA, opB, okA, okB)
		if (okA && opA.SettleTime != nil) || (okB && opB.SettleTime != nil) {
			writeRow(&sb, rng, name, "settle", opA, opB, okA, okB)
		}
	}

	return sb.String()
}

func writeRow(sb *strings.Builder, rng *rand.Rand, op, metric string, opA, opB *OpResult, okA, okB bool) {
	sa := seriesFor(opA, metric)
	sb2 := seriesFor(opB, metric)

	if sa == nil || sb2 == nil || sa.Count == 0 || sb2.Count == 0 {
		fmt.Fprintf(sb, "| %s | %s | %s | %s | n/a | n/a | %s | %s | %s | %s | %s | %s | n/a | missing on one side |\n",
			op, metric,
			naf(sa, "median"), naf(sb2, "median"),
			naf(sa, "p95"), naf(sb2, "p95"),
			nai(sa), nai(sb2),
			errRate(opA, okA), errRate(opB, okB))
		return
	}

	delta := sb2.Median - sa.Median
	pct := 0.0
	if sa.Median != 0 {
		pct = delta / sa.Median * 100
	}

	loCI, hiCI, ok := bootstrapMedianDiffCI(sa.Samples, sb2.Samples, rng)
	ciStr := "n/a"
	signal := "insufficient data"
	if ok {
		ciStr = fmt.Sprintf("[%.2f, %.2f]", loCI, hiCI)
		if loCI > 0 || hiCI < 0 {
			signal = "likely real (CI excludes 0)"
		} else {
			signal = "indicative only (CI includes 0)"
		}
	}

	p95Note := ""
	if !sa.P95Reliable || !sb2.P95Reliable {
		p95Note = "*"
	}

	fmt.Fprintf(sb, "| %s | %s | %.2f | %.2f | %+.2f | %+.1f%% | %.2f%s | %.2f%s | %d | %d | %s | %s | %s | %s |\n",
		op, metric, sa.Median, sb2.Median, delta, pct,
		sa.P95, p95Note, sb2.P95, p95Note,
		sa.Count, sb2.Count,
		errRate(opA, okA), errRate(opB, okB),
		ciStr, signal)
}

func naf(s *SeriesStats, field string) string {
	if s == nil || s.Count == 0 {
		return "n/a"
	}
	if field == "p95" {
		return fmt.Sprintf("%.2f", s.P95)
	}
	return fmt.Sprintf("%.2f", s.Median)
}

func nai(s *SeriesStats) string {
	if s == nil {
		return "0"
	}
	return strconv.Itoa(s.Count)
}

func errRate(op *OpResult, present bool) string {
	if !present || op == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", op.ErrorRate*100)
}
