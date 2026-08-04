package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// handleAttachVolume validates the volume request against the volume
// service (existence, ownership, availability, AZ) and dispatches the
// QMP/state-machine pipeline to vm.Manager.AttachVolume. The manager owns
// every QMP and persistence side-effect; the daemon only emits the AWS API
// response.
func (d *Daemon) handleAttachVolume(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	slog.InfoContext(ctx, "Attaching volume to instance", "instanceId", command.ID)

	if command.AttachVolumeData == nil || command.AttachVolumeData.VolumeID == "" {
		slog.ErrorContext(ctx, "AttachVolume: missing attach volume data")
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	volumeID := command.AttachVolumeData.VolumeID

	// Surface IncorrectInstanceState before any volume-side lookups so the
	// caller sees the instance-state error first when both apply.
	status := d.vmMgr.Status(instance)
	if status != vm.StateRunning {
		slog.ErrorContext(ctx, "AttachVolume: instance not running",
			"instanceId", command.ID, "status", status)
		respondWithError(msg, awserrors.ErrorIncorrectInstanceState)
		return
	}

	volCfg, err := d.volumeService.GetVolumeConfig(volumeID)
	if err != nil {
		slog.ErrorContext(ctx, "AttachVolume: failed to get volume config", "volumeId", volumeID, "err", err)
		respondWithError(msg, awserrors.ErrorInvalidVolumeNotFound)
		return
	}

	callerAccountID := utils.AccountIDFromMsg(msg)
	if !volumeVisibleTo(volCfg.VolumeMetadata.TenantID, callerAccountID) {
		slog.WarnContext(ctx, "AttachVolume: account does not own volume",
			"volumeId", volumeID,
			"callerAccount", callerAccountID,
			"ownerAccount", volCfg.VolumeMetadata.TenantID)
		respondWithError(msg, awserrors.ErrorInvalidVolumeNotFound)
		return
	}

	if volCfg.VolumeMetadata.State != "available" {
		if volCfg.VolumeMetadata.AttachedInstance == command.ID {
			if command.AttachVolumeData.Device != "" &&
				command.AttachVolumeData.Device != volCfg.VolumeMetadata.DeviceName {
				slog.ErrorContext(ctx, "AttachVolume: requested device conflicts with existing attachment",
					"volumeId", volumeID, "instanceId", command.ID,
					"requestedDevice", command.AttachVolumeData.Device,
					"attachedDevice", volCfg.VolumeMetadata.DeviceName)
				// AWS returns VolumeInUse (not InvalidParameterValue) when a
				// re-attach targets a device other than the one already in use.
				respondWithError(msg, awserrors.ErrorVolumeInUse)
				return
			}

			// Volume is already attached to this instance (e.g. a CSI
			// ControllerPublishVolume retry after a slow first attach).
			// Treat as an idempotent success instead of VolumeInUse.
			slog.InfoContext(ctx, "AttachVolume: volume already attached to requesting instance, returning idempotent success",
				"volumeId", volumeID, "instanceId", command.ID, "device", volCfg.VolumeMetadata.DeviceName)
			d.respondWithVolumeAttachment(msg, volumeID, command.ID, volCfg.VolumeMetadata.DeviceName, "attached")
			return
		}

		slog.ErrorContext(ctx, "AttachVolume: volume not available",
			"volumeId", volumeID, "state", volCfg.VolumeMetadata.State)
		respondWithError(msg, awserrors.ErrorVolumeInUse)
		return
	}

	if volCfg.VolumeMetadata.AvailabilityZone != "" && d.config.AZ != "" &&
		volCfg.VolumeMetadata.AvailabilityZone != d.config.AZ {
		slog.ErrorContext(ctx, "AttachVolume: volume and instance are in different AZs",
			"volumeId", volumeID,
			"volumeAZ", volCfg.VolumeMetadata.AvailabilityZone,
			"instanceAZ", d.config.AZ)
		respondWithError(msg, awserrors.ErrorInvalidVolumeZoneMismatch)
		return
	}

	device, err := d.vmMgr.AttachVolume(ctx, instance.ID, volumeID, command.AttachVolumeData.Device)
	if err != nil {
		respondWithError(msg, attachDetachErrorCode(err))
		return
	}

	// AttachVolume returns the API-form device name (/dev/sd[f-p]), not
	// the in-guest path. Callers (including the Terraform AWS provider)
	// round-trip this through DescribeVolumes' attachment.device filter,
	// which only matches the API-form name persisted in volume metadata.
	// A guest-form name here breaks the immediate post-attach wait loop.
	// This diverges intentionally from BlockDeviceMappings, which retain
	// the guest path for in-guest device discovery.
	d.respondWithVolumeAttachment(msg, volumeID, command.ID, device, "attached")
}

// handleDetachVolume dispatches the QMP/state-machine pipeline to
// vm.Manager.DetachVolume and emits the AWS API response.
func (d *Daemon) handleDetachVolume(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	slog.InfoContext(ctx, "Detaching volume from instance", "instanceId", command.ID)

	if command.DetachVolumeData == nil || command.DetachVolumeData.VolumeID == "" {
		slog.ErrorContext(ctx, "DetachVolume: missing detach volume data")
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	deviceName, err := d.vmMgr.DetachVolume(
		ctx,
		instance.ID,
		command.DetachVolumeData.VolumeID,
		command.DetachVolumeData.Device,
		command.DetachVolumeData.Force,
	)
	if err != nil {
		respondWithError(msg, attachDetachErrorCode(err))
		return
	}

	d.respondWithVolumeAttachment(msg, command.DetachVolumeData.VolumeID, command.ID, deviceName, "detaching")
}

// handleDrainVolume flushes the volume's in-flight writes to S3 by dialing the
// drain socket the local NBD plugin serves. ec2.CreateSnapshot is answered by
// any node in the cluster, but only the node hosting the instance subscribes
// ec2.cmd.{instanceID}, so routing the drain here lands it where the writes are.
//
// Runs on its own goroutine (see handleEC2Events) so a long flush cannot hold
// the instance's command subscription.
func (d *Daemon) handleDrainVolume(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	ctx, span := startOpSpan(ctx, "ec2.DrainVolume", command.ID)
	var err error
	defer func() { endOpSpan(span, err) }()

	if command.DrainVolumeData == nil || command.DrainVolumeData.VolumeID == "" {
		slog.ErrorContext(ctx, "DrainVolume: missing drain volume data", "instanceId", command.ID)
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	volumeID := command.DrainVolumeData.VolumeID

	// Only stable post-teardown states prove the volume was sealed. Transitional
	// states still have a live or shutting-down writer, so they must drain or
	// fail rather than acknowledge an older checkpoint as current.
	status := d.vmMgr.Status(instance)
	if status == vm.StateStopped || status == vm.StateTerminated {
		slog.InfoContext(ctx, "DrainVolume: instance teardown is complete, nothing to drain",
			"volumeId", volumeID, "instanceId", command.ID, "status", status)
		respondWithJSON(msg, types.DrainVolumeResponse{VolumeID: volumeID, Status: types.DrainVolumeStatusNotRunning})
		return
	}

	slog.InfoContext(ctx, "Draining volume before snapshot", "volumeId", volumeID, "instanceId", command.ID)

	// A missing or unresponsive socket under a running instance means the writes
	// cannot be made current, so the caller must fail its snapshot rather than
	// read a stale checkpoint.
	if err = handlers_ec2_snapshot.DrainVolumeSocket(d.config.DataDir, volumeID); err != nil {
		slog.ErrorContext(ctx, "DrainVolume: drain failed", "volumeId", volumeID, "instanceId", command.ID, "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	respondWithJSON(msg, types.DrainVolumeResponse{VolumeID: volumeID, Status: types.DrainVolumeStatusDrained})
}

// attachDetachErrorCode maps a vm.Manager error returned by AttachVolume
// or DetachVolume to the AWS API error code that the SDK expects.
func attachDetachErrorCode(err error) string {
	switch {
	case errors.Is(err, vm.ErrInstanceNotFound):
		return awserrors.ErrorInvalidInstanceIDNotFound
	case errors.Is(err, vm.ErrInvalidTransition):
		return awserrors.ErrorIncorrectInstanceState
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
func (d *Daemon) handleEC2ModifyVolume(msg *nats.Msg) {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()

	slog.DebugContext(ctx, "Received message", "subject", msg.Subject)
	slog.DebugContext(ctx, "Message data", "data", string(msg.Data))

	accountID := utils.AccountIDFromMsg(msg)

	modifyVolumeInput := &ec2.ModifyVolumeInput{}
	errResp := utils.UnmarshalJsonPayload(modifyVolumeInput, msg.Data)

	if errResp != nil {
		utils.MarkSpanError(span, errors.New(awserrors.ErrorInvalidParameterValue))
		if err := msg.Respond(errResp); err != nil {
			slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
		}
		slog.ErrorContext(ctx, "Request does not match ModifyVolumeInput")
		return
	}

	slog.InfoContext(ctx, "Processing ModifyVolume request", "volumeId", modifyVolumeInput.VolumeId, "accountID", accountID)

	output, err := d.volumeService.ModifyVolume(ctx, modifyVolumeInput, accountID)

	if err != nil {
		slog.ErrorContext(ctx, "handleEC2ModifyVolume service.ModifyVolume failed", "err", err)
		utils.MarkSpanError(span, err)
		respondWithServiceError(msg, err)
		return
	}

	respondWithJSON(msg, output)

	// Notify viperblockd to reload state after volume modification (e.g. resize)
	if modifyVolumeInput.VolumeId != nil {
		syncData, err := json.Marshal(types.EBSSyncRequest{Volume: *modifyVolumeInput.VolumeId})
		if err != nil {
			slog.ErrorContext(ctx, "failed to marshal ebs.sync request", "volumeId", *modifyVolumeInput.VolumeId, "err", err)
		} else {
			_, syncErr := d.natsConn.Request("ebs.sync", syncData, 5*time.Second)
			if syncErr != nil {
				slog.WarnContext(ctx, "ebs.sync notification failed (volume may not be mounted)",
					"volumeId", *modifyVolumeInput.VolumeId, "err", syncErr)
			}
		}
	}

	slog.InfoContext(ctx, "handleEC2ModifyVolume completed", "volumeId", modifyVolumeInput.VolumeId)
}
