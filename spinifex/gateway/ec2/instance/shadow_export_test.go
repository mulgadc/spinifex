package gateway_ec2_instance

import "time"

// NewShadowComparatorForTest builds a ShadowComparator with an arbitrarily
// small concurrency budget, so a test can force the "skipped, budget
// exhausted" path without spawning maxConcurrentShadowRuns goroutines.
func NewShadowComparatorForTest(resyncInterval time.Duration, maxConcurrent int) *ShadowComparator {
	if resyncInterval <= 0 {
		resyncInterval = describeShadowResyncInterval
	}
	return &ShadowComparator{
		tracker:        newDivergenceTracker(),
		resyncInterval: resyncInterval,
		slots:          make(chan struct{}, maxConcurrent),
	}
}
