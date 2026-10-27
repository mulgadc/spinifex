package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// RequestZoneReload asks every northstar server to reload zone and collects
// their acknowledgements until want have arrived or ctx ends. The acks gathered
// so far are returned either way; the error reports only a transport failure.
func RequestZoneReload(ctx context.Context, nc *nats.Conn, zone string, want int) ([]ZoneReloadAck, error) {
	payload, err := json.Marshal(ZoneReload{Zone: zone})
	if err != nil {
		return nil, fmt.Errorf("marshal zone reload: %w", err)
	}
	inbox := nc.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("subscribe reload acks: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.PublishRequest(SubjectZoneReload, inbox, payload); err != nil {
		return nil, fmt.Errorf("publish zone reload: %w", err)
	}

	var acks []ZoneReloadAck
	for len(acks) < want {
		msg, err := sub.NextMsgWithContext(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return acks, nil
			}
			return acks, fmt.Errorf("receive reload ack: %w", err)
		}
		var ack ZoneReloadAck
		if json.Unmarshal(msg.Data, &ack) != nil || ack.Zone != zone {
			continue
		}
		acks = append(acks, ack)
	}
	return acks, nil
}
