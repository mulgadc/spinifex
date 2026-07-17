package dns

import (
	"context"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// PublishChanges sends a batch of record-set changes to the writer via NATS
// request-reply and waits for the ack. It is best-effort from the caller's
// perspective: a nil connection or empty batch is a no-op, and the error is
// returned for the caller to log without failing the resource operation.
func PublishChanges(nc *nats.Conn, accountID string, changes []Change) (*ChangeResult, error) {
	if nc == nil || len(changes) == 0 {
		return &ChangeResult{}, nil
	}
	// Detached from any request context: the publish is best-effort and its own
	// timeout bounds it, so a cancelled caller ctx must not abort the write.
	return utils.NATSRequest[ChangeResult](context.Background(), nc, SubjectRecordsetChange, ChangeBatch{Changes: changes}, requestTimeout, accountID)
}

// PublishChangesBestEffort publishes a batch and logs the outcome without
// propagating the error, so DNS registration never blocks a resource operation.
// The reconcile loop repairs anything missed by a failed publish.
func PublishChangesBestEffort(nc *nats.Conn, accountID string, changes []Change) {
	if len(changes) == 0 {
		return
	}
	if res, err := PublishChanges(nc, accountID, changes); err != nil {
		slog.Warn("dns: publish changes failed (continuing)", "count", len(changes), "error", err)
	} else {
		slog.Info("dns: published changes", "count", len(changes), "zones", res.Zones)
	}
}
