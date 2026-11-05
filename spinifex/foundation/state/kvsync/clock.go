package kvsync

import "time"

// clock is the monotonic time source a Lease measures expiry and renewal on.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	// Deadline is After with a stop, for a bound that is usually not reached.
	Deadline(d time.Duration) (<-chan time.Time, func())
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) Deadline(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}
