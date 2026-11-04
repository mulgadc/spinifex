// Package backoff holds the jitter and context-aware sleep shared by the
// external-network allocators' retry and poll loops.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// Jitter returns a uniform offset in [-span, span), or 0 when span <= 0.
func Jitter(span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(2*span))) - span //nolint:gosec // jitter, not a secret
}

// Sleep waits for d or until ctx is done, returning ctx.Err() in the latter
// case. A non-positive d still reports an already-cancelled ctx.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
