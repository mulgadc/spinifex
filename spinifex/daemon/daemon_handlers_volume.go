package daemon

import (
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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
func (d *Daemon) handleAttachVolume(msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	slog.Info("Attaching volume to instance", "instanceId", command.ID)

	if command.AttachVolumeData == nil || command.AttachVolumeData.VolumeID == "" {
		slog.Error("AttachVolume: missing attach volume data")
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	volumeID := command.AttachVolumeData.VolumeID

	// Surface IncorrectInstanceState before any volume-side lookups so the
	// caller sees the instance-state error first when both apply.
	status := d.vmMgr.Status(instance)
	if status != vm.StateRunning {
		slog.Error("AttachVolume: instance not running",
			"instanceId", command.ID, "status", status)
		respondWithError(msg, awserrors.ErrorIncorrectInstanceState)
		return
	}

	volCfg, err := d.volumeService.GetVolumeConfig(volumeID)
	if err != nil {
		slog.Error("AttachVolume: failed to get volume config", "volumeId", volumeID, "err", err)
		respondWithError(msg, awserrors.ErrorInvalidVolumeNotFound)
		return
	}

	callerAccountID := utils.AccountIDFromMsg(msg)
	if !volumeVisibleTo(volCfg.VolumeMetadata.TenantID, callerAccountID) {
		slog.Warn("AttachVolume: account does not own volume",
			"volumeId", volumeID,
			"callerAccount", callerAccountID,
			"ownerAccount", volCfg.VolumeMetadata.TenantID)
		respondWithError(msg, awserrors.ErrorInvalidVolumeNotFound)
		return
	}

	if volCfg.VolumeMetadata.State != "available" {
		slog.Error("AttachVolume: volume not available",
			"volumeId", volumeID, "state", volCfg.VolumeMetadata.State)
		respondWithError(msg, awserrors.ErrorVolumeInUse)
		return
	}

	if volCfg.VolumeMetadata.AvailabilityZone != "" && d.config.AZ != "" &&
		volCfg.VolumeMetadata.AvailabilityZone != d.config.AZ {
		slog.Error("AttachVolume: volume and instance are in different AZs",
			"volumeId", volumeID,
			"volumeAZ", volCfg.VolumeMetadata.AvailabilityZone,
			"instanceAZ", d.config.AZ)
		respondWithError(msg, awserrors.ErrorInvalidVolumeZoneMismatch)
		return
	}

	res, err := d.vmMgr.AttachVolume(instance.ID, volumeID, command.AttachVolumeData.Device)
	if err != nil {
		respondWithError(msg, attachDetachErrorCode(err))
		return
	}

	d.respondWithVolumeAttachment(msg, volumeID, command.ID, res.GuestDevice, "attached")
}

// handleDetachVolume dispatches the QMP/state-machine pipeline to
// vm.Manager.DetachVolume and emits the AWS API response.
func (d *Daemon) handleDetachVolume(msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	slog.Info("Detaching volume from instance", "instanceId", command.ID)

	if command.DetachVolumeData == nil || command.DetachVolumeData.VolumeID == "" {
		slog.Error("DetachVolume: missing detach volume data")
		respondWithError(msg, awserrors.ErrorInvalidParameterValue)
		return
	}

	deviceName, err := d.vmMgr.DetachVolume(
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

func (d *Daemon) handleEC2CreateVolume(msg *nats.Msg) {
	handleNATSRequest(msg, d.volumeService.CreateVolume)
}

func (d *Daemon) handleEC2DescribeVolumes(msg *nats.Msg) {
	handleNATSRequest(msg, d.volumeService.DescribeVolumes)
}

func (d *Daemon) handleEC2DescribeVolumeStatus(msg *nats.Msg) {
	handleNATSRequest(msg, d.volumeService.DescribeVolumeStatus)
}

func (d *Daemon) handleEC2DescribeVolumesModifications(msg *nats.Msg) {
	handleNATSRequest(msg, d.volumeService.DescribeVolumesModifications)
}

// handleEC2ModifyVolume processes incoming EC2 ModifyVolume requests
func (d *Daemon) handleEC2ModifyVolume(msg *nats.Msg) {
	slog.Debug("Received message", "subject", msg.Subject)
	slog.Debug("Message data", "data", string(msg.Data))

	accountID := utils.AccountIDFromMsg(msg)

	modifyVolumeInput := &ec2.ModifyVolumeInput{}
	errResp := utils.UnmarshalJsonPayload(modifyVolumeInput, msg.Data)

	if errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		slog.Error("Request does not match ModifyVolumeInput")
		return
	}

	slog.Info("Processing ModifyVolume request", "volumeId", modifyVolumeInput.VolumeId, "accountID", accountID)

	output, err := d.volumeService.ModifyVolume(modifyVolumeInput, accountID)

	if err != nil {
		slog.Error("handleEC2ModifyVolume service.ModifyVolume failed", "err", err)
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}

	respondWithJSON(msg, output)

	// Notify viperblockd to reload state after volume modification (e.g. resize)
	if modifyVolumeInput.VolumeId != nil {
		syncData, err := json.Marshal(types.EBSSyncRequest{Volume: *modifyVolumeInput.VolumeId})
		if err != nil {
			slog.Error("failed to marshal ebs.sync request", "volumeId", *modifyVolumeInput.VolumeId, "err", err)
		} else {
			_, syncErr := d.natsConn.Request("ebs.sync", syncData, 5*time.Second)
			if syncErr != nil {
				slog.Warn("ebs.sync notification failed (volume may not be mounted)",
					"volumeId", *modifyVolumeInput.VolumeId, "err", syncErr)
			}
		}
	}

	slog.Info("handleEC2ModifyVolume completed", "volumeId", modifyVolumeInput.VolumeId)
}

func (d *Daemon) handleEC2DeleteVolume(msg *nats.Msg) {
	handleNATSRequest(msg, d.volumeService.DeleteVolume)
}
