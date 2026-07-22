package dhcp

import (
	"context"
	"testing"
	"time"
)

// socketTimeout must never cap the caller's budget, because nclient4 ends an
// attempt on whichever of the two deadlines fires first.
func TestSocketTimeoutTracksContextDeadline(t *testing.T) {
	c := NewNClient4(5 * time.Second)

	t.Run("no deadline falls back to the configured timeout", func(t *testing.T) {
		if got := c.socketTimeout(context.Background()); got != 5*time.Second {
			t.Fatalf("socketTimeout = %v, want 5s", got)
		}
	})

	t.Run("longer deadline is honoured rather than capped", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 32*time.Second)
		defer cancel()
		got := c.socketTimeout(ctx)
		if got <= 5*time.Second {
			t.Fatalf("socketTimeout = %v, want the remaining ~32s, not the 5s fallback", got)
		}
		// Strictly beyond the caller's deadline so ctx.Done() reports the timeout.
		if got <= 32*time.Second {
			t.Fatalf("socketTimeout = %v, want more than the remaining 32s", got)
		}
	})

	t.Run("shorter deadline shortens the read", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		got := c.socketTimeout(ctx)
		if got <= time.Second || got > 2*time.Second {
			t.Fatalf("socketTimeout = %v, want just beyond the remaining 1s", got)
		}
	})

	t.Run("expired deadline yields a positive timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		// nclient4 rejects a non-positive read deadline; ctx.Done() ends the
		// attempt immediately regardless of what is passed here.
		if got := c.socketTimeout(ctx); got != 5*time.Second {
			t.Fatalf("socketTimeout = %v, want the 5s fallback", got)
		}
	})
}
