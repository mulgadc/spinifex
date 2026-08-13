package ebsctl

import (
	"math"
	"sort"
)

// percentile returns the p-th percentile (0<=p<=1) of sorted using linear
// interpolation between closest ranks (the common "R-7" convention, same as
// numpy's default). sorted must already be ascending.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := p * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

// median is percentile(sorted, 0.5).
func median(sorted []float64) float64 {
	return percentile(sorted, 0.5)
}

// computeSeries reduces samples (in the order they were recorded) to a
// SeriesStats: the raw Samples field preserves recording order (not sorted)
// so a reader can see arrival-order trends; every statistic is computed off
// a sorted copy.
func computeSeries(samples []float64) SeriesStats {
	n := len(samples)
	if n == 0 {
		return SeriesStats{Count: 0, Samples: []float64{}}
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)

	sum := 0.0
	for _, v := range sorted {
		sum += v
	}

	return SeriesStats{
		Count:       n,
		Median:      median(sorted),
		P95:         percentile(sorted, 0.95),
		P95Reliable: n >= MinSamplesForP95,
		Mean:        sum / float64(n),
		Min:         sorted[0],
		Max:         sorted[n-1],
		Samples:     samples,
	}
}
