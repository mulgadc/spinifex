package gateway_ec2_instance

import "time"

// NewShadowComparatorForTest builds a ShadowComparator with an arbitrarily
// small concurrency budget and live-node persistence deadline, so a test can
// force the "skipped, budget exhausted" path or cross a live-node deadline
// without waiting on instancecache.NodeStaleAfter in real time. A
// non-positive liveNodeDeadline falls back to the real default.
func NewShadowComparatorForTest(resyncInterval, liveNodeDeadline time.Duration, maxConcurrent int) *ShadowComparator {
	if resyncInterval <= 0 {
		resyncInterval = describeShadowResyncInterval
	}
	if liveNodeDeadline <= 0 {
		liveNodeDeadline = describeShadowLiveNodePersistenceDeadline
	}
	return &ShadowComparator{
		tracker:          newDivergenceTracker(),
		resyncInterval:   resyncInterval,
		liveNodeDeadline: liveNodeDeadline,
		slots:            make(chan struct{}, maxConcurrent),
	}
}

// LiveNodePersistenceDeadlineForTest exposes the default live-node
// persistence deadline so a test can compare it against
// instancecache.NodeStaleAfter directly, without waiting on it.
func LiveNodePersistenceDeadlineForTest() time.Duration {
	return describeShadowLiveNodePersistenceDeadline
}
