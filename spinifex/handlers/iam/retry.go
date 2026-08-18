package handlers_iam

import (
	"context"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// NewIAMServiceWithRetry builds an IAMServiceImpl, retrying while its NATS
// JetStream KV backend is unavailable. On concurrent multi-node boot the KV
// store needs cluster quorum that may not exist yet; a single attempt would
// leave the service nil for the process lifetime. Blocks up to maxWait, then
// returns the last error. Callers that legitimately run without a master key
// must guard the call themselves.
func NewIAMServiceWithRetry(ctx context.Context, natsConn *nats.Conn, masterKey []byte, clusterSize int) (*IAMServiceImpl, error) {
	const maxWait = 5 * time.Minute
	retryDelay := 500 * time.Millisecond
	start := time.Now()
	attempt := 0

	for {
		attempt++
		svc, err := NewIAMServiceImpl(ctx, natsConn, masterKey, clusterSize)
		if err == nil {
			if attempt > 1 {
				slog.Info("IAM service initialized after retry", "attempts", attempt, "elapsed_ms", otelsetup.Millis(time.Since(start)))
			}
			return svc, nil
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
