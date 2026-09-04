package kvstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
)

// DefaultOpenWindow is how long OpenWithRetry keeps trying. Long enough to
// cover JetStream electing a leader after a whole-cluster restart, which is the
// window in which every service starts at once and races it.
const DefaultOpenWindow = 90 * time.Second

// openRetryInterval is the pause between attempts. Each attempt carries its own
// JetStream API deadline, so this only adds to it. A var so tests can shorten
// it; nothing in production writes it.
var openRetryInterval = 2 * time.Second

// OpenWithRetry calls open until it succeeds or window expires, and reports how
// long it waited. A bucket that is merely not ready yet is the common case on a
// cold cluster; the caller decides what an exhausted window means.
func OpenWithRetry[T any](ctx context.Context, what string, window time.Duration, open func(context.Context) (T, error)) (T, error) {
	var zero T
	if window <= 0 {
		window = DefaultOpenWindow
	}

	started := time.Now()
	deadline := started.Add(window)
	attempt := 0

	for {
		attempt++
		v, err := open(ctx)
		if err == nil {
			if attempt > 1 {
				slog.InfoContext(ctx, "kvstore: opened after retrying", "what", what,
					"attempts", attempt, "waited_ms", otelsetup.Millis(time.Since(started)))
			}
			return v, nil
		}

		if ctx.Err() != nil {
			return zero, fmt.Errorf("open %s: %w", what, ctx.Err())
		}
		if !time.Now().Add(openRetryInterval).Before(deadline) {
			return zero, fmt.Errorf("open %s: giving up after %d attempts in %s: %w",
				what, attempt, window, err)
		}

		slog.WarnContext(ctx, "kvstore: not ready, retrying", "what", what,
			"attempt", attempt, "window_ms", otelsetup.Millis(window), "err", err)

		select {
		case <-ctx.Done():
			return zero, fmt.Errorf("open %s: %w", what, ctx.Err())
		case <-time.After(openRetryInterval):
		}
	}
}
