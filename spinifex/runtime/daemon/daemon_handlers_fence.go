package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/nats-io/nats.go"
)

// fenceNoticeSubs is this node's fence-notice subscription, whatever the EBS
// provider. Node-addressed with no queue group: the fenced guest is on this
// host, so a worker on another node would find nothing to stop.
func (d *Daemon) fenceNoticeSubs() []natsSub {
	subject, err := ebsprovider.FenceNoticeSubject(d.node)
	if err != nil {
		slog.Error("Not serving EBS fence notices: node ID is not a single NATS subject token", "node", d.node, "err", err)
		return nil
	}
	return []natsSub{{subject, d.handleFenceNotice, ""}}
}

// handleFenceNotice stops the local guest whose volume the EBS provider has
// just revoked the export of.
//
// By the time this arrives the disk is already gone, so the guest is doing no
// useful work and its writes cannot reach the backend. What is left is a QEMU
// process holding memory and taps, and an instance record claiming this node
// runs something that runs somewhere else now.
func (d *Daemon) handleFenceNotice(msg *nats.Msg) string {
	ctx, span := ebsprovider.StartServerSpan(context.Background(), msg)
	defer span.End()

	var notice ebsprovider.FenceNotice
	decodeErr := json.Unmarshal(msg.Data, &notice)
	ack := ebsprovider.FenceNoticeAck{Versioned: ebsprovider.ReplyVersioned(notice.SchemaVersion)}
	refuse := func(code ebsprovider.ErrorCode, message string) string {
		slog.ErrorContext(ctx, "volume fenced: refused the notice", "volumeId", notice.VolumeID, "code", code, "err", message)
		ack.Error = &ebsprovider.ProviderError{Code: code, Message: message}
		ebsprovider.RecordProviderError(span, ack.Error)
		ackFenceNotice(ctx, msg, ack)
		return outcomeError
	}
	if decodeErr != nil {
		return refuse(ebsprovider.ErrorCodeInvalidArgument, "bad notice: "+decodeErr.Error())
	}
	if !ebsprovider.SupportsSchemaVersion(notice.SchemaVersion) {
		return refuse(ebsprovider.ErrorCodeUnsupportedVersion,
			fmt.Sprintf("unsupported schema version %d; ", notice.SchemaVersion)+ebsprovider.SupportedVersionsText())
	}
	if notice.VolumeID == "" {
		return refuse(ebsprovider.ErrorCodeInvalidArgument, "notice named no volume")
	}

	// FenceVolume moves the guest out of running before it returns, so the ack
	// below is sent only once nothing here will resume it.
	ack.InstanceID = d.vmMgr.FenceVolume(ctx, notice.VolumeID, notice.Reason)
	ackFenceNotice(ctx, msg, ack)
	if ack.InstanceID == "" {
		// The usual case: the volume outlived the guest that was using it, so
		// there is nothing local left to stop.
		slog.InfoContext(ctx, "volume fenced with no local guest using it",
			"volumeId", notice.VolumeID, "winner", notice.Winner)
		return outcomeSuccess
	}

	slog.ErrorContext(ctx, "volume fenced, stopped the guest that was using it",
		"volumeId", notice.VolumeID, "instanceId", ack.InstanceID, "winner", notice.Winner)
	return outcomeSuccess
}

// ackFenceNotice answers the provider, which repeats the notice until it hears
// back. A notice with no reply subject was published once and needs no answer.
func ackFenceNotice(ctx context.Context, msg *nats.Msg, ack ebsprovider.FenceNoticeAck) {
	if msg.Reply == "" {
		return
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		slog.ErrorContext(ctx, "volume fenced: marshal ack", "err", err)
		return
	}
	if err := msg.Respond(payload); err != nil {
		slog.WarnContext(ctx, "volume fenced: could not acknowledge, the provider will repeat the notice", "err", err)
	}
}
