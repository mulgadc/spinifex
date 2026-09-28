package handlers_iam

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"

	"github.com/nats-io/nats.go"
)

// NewIAMServiceWithRetry builds an IAMServiceImpl, retrying while its NATS
// JetStream KV backend is unavailable. On concurrent multi-node boot the KV
// store needs cluster quorum that may not exist yet; a single attempt would
// leave the service nil for the process lifetime. Blocks up to maxWait, then
// returns the last error. Callers that legitimately run without a master key
// must guard the call themselves.
func NewIAMServiceWithRetry(ctx context.Context, natsConn *nats.Conn, masterKey []byte) (*IAMServiceImpl, error) {
	const maxWait = 5 * time.Minute
	retryDelay := 500 * time.Millisecond
	start := time.Now()
	attempt := 0

	for {
		attempt++
		svc, err := NewIAMServiceImpl(ctx, natsConn, masterKey)
		if err == nil {
			if attempt > 1 {
				slog.Info("IAM service initialized after retry", "attempts", attempt, "elapsed_ms", otelsetup.Millis(time.Since(start)))
			}
			return svc, nil
		}

		// An undeclared cluster size is a configuration fault, not a cluster that
		// has yet to form, so retrying it turns a clear error into five minutes
		// of waiting followed by a misleading one.
		if clustersize.Permanent(err) {
			return nil, err
		}

		elapsed := time.Since(start)
		if elapsed >= maxWait {
			return nil, fmt.Errorf("IAM service unavailable after %s (%d attempts): %w", elapsed.Round(time.Second), attempt, err)
		}

		slog.Warn("IAM service not ready (waiting for JetStream cluster quorum)", "error", err, "attempt", attempt, "elapsed_ms", otelsetup.Millis(elapsed), "retry_in_ms", otelsetup.Millis(retryDelay))
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, 10*time.Second)
	}
}
