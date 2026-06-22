package dns

import (
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
	return utils.NATSRequest[ChangeResult](nc, SubjectRecordsetChange, ChangeBatch{Changes: changes}, requestTimeout, accountID)
}

// PublishEC2 builds and publishes the public+private record changes for one
// instance. Failures are logged, not propagated, so DNS registration never
// blocks a launch or terminate.
func PublishEC2(nc *nats.Conn, accountID string, action Action, region, baseDomain, publicIP, privateIP string) {
	changes := EC2Changes(action, region, baseDomain, publicIP, privateIP)
	if len(changes) == 0 {
		return
	}
	if _, err := PublishChanges(nc, accountID, changes); err != nil {
		slog.Warn("dns: publish ec2 records failed (continuing)",
			"action", action, "publicIp", publicIP, "privateIp", privateIP, "error", err)
		return
	}
	slog.Info("dns: published ec2 records", "action", action, "publicIp", publicIP, "privateIp", privateIP)
}
