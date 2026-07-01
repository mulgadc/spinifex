package handlers_iam

import (
	"fmt"
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
func NewIAMServiceWithRetry(natsConn *nats.Conn, masterKey []byte, clusterSize int) (*IAMServiceImpl, error) {
	const maxWait = 5 * time.Minute
	retryDelay := 500 * time.Millisecond
	start := time.Now()
	attempt := 0

	for {
		attempt++
		svc, err := NewIAMServiceImpl(natsConn, masterKey, clusterSize)
		if err == nil {
			if attempt > 1 {
				slog.Info("IAM service initialized after retry", "attempts", attempt, "elapsed", time.Since(start).Round(time.Second))
			}
			return svc, nil
		}

		elapsed := time.Since(start)
		if elapsed >= maxWait {
			return nil, fmt.Errorf("IAM service unavailable after %s (%d attempts): %w", elapsed.Round(time.Second), attempt, err)
		}

		slog.Warn("IAM service not ready (waiting for JetStream cluster quorum)", "error", err, "attempt", attempt, "elapsed", elapsed.Round(time.Second), "retryIn", retryDelay)
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, 10*time.Second)
	}
}
