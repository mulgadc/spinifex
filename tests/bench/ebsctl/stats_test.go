package ebsctl

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestPercentile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		p    float64
		want float64
	}{
		{0, 1},
		{1, 10},
		{0.5, 5.5},
	}
	for _, c := range cases {
		got := percentile(sorted, c.p)
		if !almostEqual(got, c.want) {
			t.Errorf("percentile(%v, %v) = %v, want %v", sorted, c.p, got, c.want)
		}
	}
}

func TestPercentileSingleValue(t *testing.T) {
	if got := percentile([]float64{42}, 0.95); got != 42 {
		t.Errorf("percentile single value = %v, want 42", got)
	}
}

func TestPercentileEmpty(t *testing.T) {
	if got := percentile(nil, 0.95); got != 0 {
		t.Errorf("percentile empty = %v, want 0", got)
	}
}

func TestMedianOddEven(t *testing.T) {
	odd := []float64{1, 2, 3, 4, 5}
	if got := median(odd); !almostEqual(got, 3) {
		t.Errorf("median(odd) = %v, want 3", got)
	}
	even := []float64{1, 2, 3, 4}
	if got := median(even); !almostEqual(got, 2.5) {
		t.Errorf("median(even) = %v, want 2.5", got)
	}
}

func TestComputeSeriesBasic(t *testing.T) {
	samples := []float64{10, 20, 30, 40, 50}
	s := computeSeries(samples)
	if s.Count != 5 {
		t.Errorf("Count = %d, want 5", s.Count)
	}
	if !almostEqual(s.Median, 30) {
		t.Errorf("Median = %v, want 30", s.Median)
	}
	if !almostEqual(s.Mean, 30) {
		t.Errorf("Mean = %v, want 30", s.Mean)
	}
	if s.Min != 10 || s.Max != 50 {
		t.Errorf("Min/Max = %v/%v, want 10/50", s.Min, s.Max)
	}
	if s.P95Reliable {
		t.Errorf("P95Reliable = true for n=5, want false (< MinSamplesForP95)")
	}
	// Samples must preserve recording (input) order, not be sorted.
	if s.Samples[0] != 10 || s.Samples[4] != 50 {
		t.Errorf("Samples order not preserved: %v", s.Samples)
	}
}

func TestComputeSeriesEmpty(t *testing.T) {
	s := computeSeries(nil)
	if s.Count != 0 {
		t.Errorf("Count = %d, want 0", s.Count)
	}
}

func TestComputeSeriesP95ReliableThreshold(t *testing.T) {
	samples := make([]float64, MinSamplesForP95)
	for i := range samples {
		samples[i] = float64(i)
	}
	s := computeSeries(samples)
	if !s.P95Reliable {
		t.Errorf("P95Reliable = false for n=%d, want true (== MinSamplesForP95)", MinSamplesForP95)
	}

	s2 := computeSeries(samples[:MinSamplesForP95-1])
	if s2.P95Reliable {
		t.Errorf("P95Reliable = true for n=%d, want false (< MinSamplesForP95)", MinSamplesForP95-1)
	}
}
