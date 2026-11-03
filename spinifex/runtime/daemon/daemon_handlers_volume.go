package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
)

// handleAttachVolume validates the volume request against the volume
// service (existence, ownership, availability, AZ) and dispatches the
// QMP/state-machine pipeline to vm.Manager.AttachVolume. The manager owns
// every QMP and persistence side-effect; the daemon only emits the AWS API
// response.
func (d *Daemon) handleAttachVolume(ctx context.Context, msg *nats.Msg, command ec2v1.EC2InstanceCommand, instance *vm.VM) string {
	slog.InfoContext(ctx, "Attaching volume to instance", "instanceId", command.ID)

	if command.AttachVolumeData == nil || command.AttachVolumeData.VolumeID == "" {
		slog.ErrorContext(ctx, "AttachVolume: missing attach volume data")
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidParameterValue)
	}

	volumeID := command.AttachVolumeData.VolumeID

	// Surface IncorrectInstanceState before any volume-side lookups so the
	// caller sees the instance-state error first when both apply.
	status := d.vmMgr.Status(instance)
	if status != vm.StateRunning {
		slog.ErrorContext(ctx, "AttachVolume: instance not running",
			"instanceId", command.ID, "status", status)
		return respondErrorOutcome(d.node, msg, awserrors.ErrorIncorrectInstanceState)
	}

	// The caller's account is a key segment, so it is needed before the read
	// rather than after it.
	callerAccountID := natsmsg.AccountIDFromMsg(msg)

	volMeta, err := d.volumeService.GetVolumeMetadata(callerAccountID, volumeID)
	if err != nil {
		slog.ErrorContext(ctx, "AttachVolume: failed to get volume metadata", "volumeId", volumeID, "err", err)
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidVolumeNotFound)
	}

	// The read was keyed on the caller's account, so the document is theirs by
	// construction and an empty caller never got this far. The comparison stays
	// as an assertion.
	if volMeta.TenantID != callerAccountID {
		slog.WarnContext(ctx, "AttachVolume: account does not own volume",
			"volumeId", volumeID,
			"callerAccount", callerAccountID,
			"ownerAccount", volMeta.TenantID)
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidVolumeNotFound)
	}

	if volMeta.State != "available" {
		if volMeta.AttachedInstance == command.ID {
			if command.AttachVolumeData.Device != "" &&
				command.AttachVolumeData.Device != volMeta.DeviceName {
				slog.ErrorContext(ctx, "AttachVolume: requested device conflicts with existing attachment",
					"volumeId", volumeID, "instanceId", command.ID,
					"requestedDevice", command.AttachVolumeData.Device,
					"attachedDevice", volMeta.DeviceName)
				// AWS returns VolumeInUse (not InvalidParameterValue) when a
				// re-attach targets a device other than the one already in use.
				return respondErrorOutcome(d.node, msg, awserrors.ErrorVolumeInUse)
			}

			// Volume is already attached to this instance (e.g. a CSI
			// ControllerPublishVolume retry after a slow first attach).
			// Treat as an idempotent success instead of VolumeInUse.
			slog.InfoContext(ctx, "AttachVolume: volume already attached to requesting instance, returning idempotent success",
				"volumeId", volumeID, "instanceId", command.ID, "device", volMeta.DeviceName)
			d.respondWithVolumeAttachment(msg, volumeID, command.ID, volMeta.DeviceName, "attached")
			return outcomeSuccess
		}

		slog.ErrorContext(ctx, "AttachVolume: volume not available",
			"volumeId", volumeID, "state", volMeta.State)
		return respondErrorOutcome(d.node, msg, awserrors.ErrorVolumeInUse)
	}

	if volMeta.AvailabilityZone != "" && d.config.AZ != "" &&
		volMeta.AvailabilityZone != d.config.AZ {
		slog.ErrorContext(ctx, "AttachVolume: volume and instance are in different AZs",
			"volumeId", volumeID,
			"volumeAZ", volMeta.AvailabilityZone,
			"instanceAZ", d.config.AZ)
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidVolumeZoneMismatch)
	}

	device, err := d.vmMgr.AttachVolume(ctx, instance.ID, volumeID, command.AttachVolumeData.Device)
	if err != nil {
		return respondErrorOutcome(d.node, msg, attachDetachErrorCode(err))
	}

	// AttachVolume returns the API-form device name (/dev/sd[f-p]), not
	// the in-guest path. Callers (including the Terraform AWS provider)
	// round-trip this through DescribeVolumes' attachment.device filter,
	// which only matches the API-form name persisted in volume metadata.
	// A guest-form name here breaks the immediate post-attach wait loop.
	// This diverges intentionally from BlockDeviceMappings, which retain
	// the guest path for in-guest device discovery.
	d.respondWithVolumeAttachment(msg, volumeID, command.ID, device, "attached")
	return outcomeSuccess
}

// handleDetachVolume dispatches the QMP/state-machine pipeline to
// vm.Manager.DetachVolume and emits the AWS API response.
func (d *Daemon) handleDetachVolume(ctx context.Context, msg *nats.Msg, command ec2v1.EC2InstanceCommand, instance *vm.VM) string {
	slog.InfoContext(ctx, "Detaching volume from instance", "instanceId", command.ID)

	if command.DetachVolumeData == nil || command.DetachVolumeData.VolumeID == "" {
		slog.ErrorContext(ctx, "DetachVolume: missing detach volume data")
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidParameterValue)
	}

	deviceName, err := d.vmMgr.DetachVolume(
		ctx,
		instance.ID,
		command.DetachVolumeData.VolumeID,
		command.DetachVolumeData.Device,
		command.DetachVolumeData.Force,
	)
	if err != nil {
		return respondErrorOutcome(d.node, msg, attachDetachErrorCode(err))
	}

	d.respondWithVolumeAttachment(msg, command.DetachVolumeData.VolumeID, command.ID, deviceName, "detaching")
	return outcomeSuccess
}

// attachDetachErrorCode maps a vm.Manager error returned by AttachVolume
// or DetachVolume to the AWS API error code that the SDK expects.
func attachDetachErrorCode(err error) string {
	switch {
	case errors.Is(err, vm.ErrInstanceNotFound):
		return awserrors.ErrorInvalidInstanceIDNotFound
	case errors.Is(err, vm.ErrInvalidTransition):
		return awserrors.ErrorIncorrectInstanceState
	case errors.Is(err, vm.ErrHotAttachUnsupported):
		return awserrors.ErrorUnsupportedOperation
	case errors.Is(err, vm.ErrAttachmentLimitExceeded):
		return awserrors.ErrorAttachmentLimitExceeded
	case errors.Is(err, vm.ErrVolumeNotAttached):
		return awserrors.ErrorIncorrectState
	case errors.Is(err, vm.ErrVolumeNotDetachable):
		return awserrors.ErrorOperationNotPermitted
	case errors.Is(err, vm.ErrVolumeDeviceMismatch):
		return awserrors.ErrorInvalidParameterValue
	default:
		return awserrors.ErrorServerInternal
	}
}

// handleEC2ModifyVolume processes incoming EC2 ModifyVolume requests.
func (d *Daemon) handleEC2ModifyVolume(msg *nats.Msg) string {
	ctx, span := natsmsg.StartConsumerSpan(msg)
	defer span.End()

	slog.DebugContext(ctx, "Received message", "subject", msg.Subject)
	slog.DebugContext(ctx, "Message data", "data", string(msg.Data))

	accountID := natsmsg.AccountIDFromMsg(msg)

	modifyVolumeInput := &ec2.ModifyVolumeInput{}
	errResp := awserrors.UnmarshalJsonPayload(modifyVolumeInput, msg.Data)

	if errResp != nil {
		natsmsg.MarkSpanError(span, errors.New(awserrors.ErrorInvalidParameterValue))
		if err := msg.Respond(errResp); err != nil {
			slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
		}
		slog.ErrorContext(ctx, "Request does not match ModifyVolumeInput")
		return outcomeError
	}

	slog.InfoContext(ctx, "Processing ModifyVolume request", "volumeId", modifyVolumeInput.VolumeId, "accountID", accountID)

	output, err := d.volumeService.ModifyVolume(ctx, modifyVolumeInput, accountID)

	if err != nil {
		slog.ErrorContext(ctx, "handleEC2ModifyVolume service.ModifyVolume failed", "err", err)
		natsmsg.MarkSpanError(span, err)
		respondWithServiceError(d.node, msg, err)
		return outcomeError
	}

	respondWithJSON(d.node, msg, output)

	// Notify viperblockd to reload state after volume modification (e.g. resize)
	if modifyVolumeInput.VolumeId != nil {
		syncData, err := json.Marshal(viperblocklegacyv1.EBSSyncRequest{Volume: *modifyVolumeInput.VolumeId})
		if err != nil {
			slog.ErrorContext(ctx, "failed to marshal ebs.sync request", "volumeId", *modifyVolumeInput.VolumeId, "err", err)
		} else {
			_, syncErr := d.natsConn.Request(viperblocklegacyv1.SyncSubject, syncData, 5*time.Second)
			if syncErr != nil {
				slog.WarnContext(ctx, "ebs.sync notification failed (volume may not be mounted)",
					"volumeId", *modifyVolumeInput.VolumeId, "err", syncErr)
			}
		}
	}

	slog.InfoContext(ctx, "handleEC2ModifyVolume completed", "volumeId", modifyVolumeInput.VolumeId)
	return outcomeSuccess
}
