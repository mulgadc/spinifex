package ebsctl

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func mkRun(provider string, samples map[string][]float64) *RunResult {
	ops := map[string]*OpResult{}
	for name, s := range samples {
		series := computeSeries(s)
		ops[name] = &OpResult{
			Operation:  name,
			Attempts:   len(s),
			APILatency: series,
			Errors:     ErrorTally{ByCode: map[string]int{}, ByMessage: map[string]int{}},
		}
	}
	return &RunResult{
		Meta: RunMeta{
			Timestamp:           time.Now(),
			Provider:            provider,
			ProviderSource:      "flag",
			IterationsPerWorker: len(samples),
			Concurrency:         1,
		},
		Operations: ops,
	}
}

func TestBootstrapMedianDiffCIIdenticalSamples(t *testing.T) {
	rng := rand.New(rand.NewPCG(0, 1))
	s := []float64{10, 11, 9, 10, 12, 8, 10, 11, 9, 10}
	lo, hi, ok := bootstrapMedianDiffCI(s, s, rng)
	if !ok {
		t.Fatal("bootstrapMedianDiffCI: ok = false, want true")
	}
	// Identical samples: the diff distribution should be tightly centred on 0.
	if lo > 1 || hi < -1 {
		t.Errorf("CI [%v, %v] for identical samples looks implausibly wide", lo, hi)
	}
}

func TestBootstrapMedianDiffCIClearShift(t *testing.T) {
	rng := rand.New(rand.NewPCG(0, 1))
	a := []float64{10, 11, 9, 10, 12, 8, 10, 11, 9, 10}
	b := make([]float64, len(a))
	for i, v := range a {
		b[i] = v + 100 // unmistakable shift
	}
	lo, hi, ok := bootstrapMedianDiffCI(a, b, rng)
	if !ok {
		t.Fatal("bootstrapMedianDiffCI: ok = false, want true")
	}
	if lo <= 0 {
		t.Errorf("CI [%v, %v] should exclude 0 for a 100ms shift", lo, hi)
	}
}

func TestBootstrapMedianDiffCIEmptySamples(t *testing.T) {
	rng := rand.New(rand.NewPCG(0, 1))
	if _, _, ok := bootstrapMedianDiffCI(nil, []float64{1, 2}, rng); ok {
		t.Error("ok = true for empty sample, want false")
	}
}

func TestCompareRunsRendersTableAndHandlesMissingOps(t *testing.T) {
	a := mkRun("embedded", map[string][]float64{
		"CreateVolume": {10, 12, 11, 9, 13, 10, 11, 12, 9, 10},
	})
	b := mkRun("viperblockd", map[string][]float64{
		"CreateVolume": {20, 22, 21, 19, 23, 20, 21, 22, 19, 20},
		"AttachVolume": {5, 6, 5, 7, 5}, // present only in b
	})

	md := CompareRuns(a, b, 1)

	if !strings.Contains(md, "CreateVolume") {
		t.Error("comparison missing CreateVolume row")
	}
	if !strings.Contains(md, "AttachVolume") {
		t.Error("comparison missing AttachVolume row for the op only present in B")
	}
	if !strings.Contains(md, "missing on one side") {
		t.Error("comparison should flag AttachVolume as missing on one side (A has no data)")
	}
	if !strings.Contains(md, "embedded") || !strings.Contains(md, "viperblockd") {
		t.Error("comparison header should name both providers")
	}
}

func TestCompareRunsNoP99Mentioned(t *testing.T) {
	a := mkRun("embedded", map[string][]float64{"CreateVolume": {1, 2, 3}})
	b := mkRun("viperblockd", map[string][]float64{"CreateVolume": {4, 5, 6}})
	md := CompareRuns(a, b, 1)
	if strings.Contains(strings.ToLower(md), "p99") {
		t.Error("comparison output must never mention p99 (see schema.go: not statistically supported at these sample sizes)")
	}
}
