package daemon

import (
	"encoding/json"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/services/viperblockd/vbwire"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// handleVolumeFenced stops the local guest whose volume viperblockd has just
// torn the export out from under.
//
// By the time this arrives the disk is already gone, so the guest is doing no
// useful work and its writes cannot reach the backend. What is left is a QEMU
// process holding memory and taps, and an instance record claiming this node
// runs something that runs somewhere else now.
func (d *Daemon) handleVolumeFenced(msg *nats.Msg) string {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()

	var event vbwire.VolumeFencedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		slog.ErrorContext(ctx, "volume fenced: bad event", "err", err)
		return outcomeError
	}
	if event.Volume == "" {
		slog.ErrorContext(ctx, "volume fenced: event named no volume")
		return outcomeError
	}

	instanceID := d.vmMgr.FenceVolume(ctx, event.Volume, event.Reason)
	if instanceID == "" {
		// The usual case: the volume outlived the guest that was using it, so
		// there is nothing local left to stop.
		slog.InfoContext(ctx, "volume fenced with no local guest using it",
			"volumeId", event.Volume, "winner", event.Winner)
		return outcomeSuccess
	}

	slog.ErrorContext(ctx, "volume fenced, stopped the guest that was using it",
		"volumeId", event.Volume, "instanceId", instanceID, "winner", event.Winner)
	return outcomeSuccess
}
