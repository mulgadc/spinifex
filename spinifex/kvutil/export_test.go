package kvutil

import (
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// NewRaiseThrottleForTest exposes the throttle the open path uses, so its
// behaviour can be driven by an injected clock rather than by sleeping.
func NewRaiseThrottleForTest(every time.Duration) *RaiseThrottleForTest {
	return &RaiseThrottleForTest{t: newRaiseThrottle(every)}
}

// RaiseThrottleForTest wraps the unexported throttle for the external test
// package.
type RaiseThrottleForTest struct{ t *raiseThrottle }

// Allow reports whether bucket may be attempted at now.
func (r *RaiseThrottleForTest) Allow(bucket string, now time.Time) bool {
	return r.t.allow(bucket, now)
}

// NewBucketReportForTest exposes how one stream's info becomes a report, which is
// where configured replicas and working replicas are told apart.
func NewBucketReportForTest(bucket string, want int, info *jetstream.StreamInfo) BucketReport {
	return newBucketReport(bucket, want, info)
}
