package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// handleAttachNetworkInterface updates KV first (crash-safe attaching state),
// then runs the QMP hot-plug pipeline. On QMP failure the KV record rolls
// back to available with the error persisted in LastAttachError.
func (d *Daemon) handleAttachNetworkInterface(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) string {
	slog.InfoContext(ctx, "Attaching ENI to instance", "instanceId", command.ID)

	if command.AttachENIData == nil || command.AttachENIData.NetworkInterfaceID == "" {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidParameterValue)
	}

	eniID := command.AttachENIData.NetworkInterfaceID
	deviceIndex := command.AttachENIData.DeviceIndex
	accountID := utils.AccountIDFromMsg(msg)

	if status := d.vmMgr.Status(instance); status != vm.StateRunning {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorIncorrectInstanceState)
	}

	record, err := d.vpcService.GetENIRecord(accountID, eniID)
	if err != nil {
		return respondServiceErrorOutcome(d.node, msg, err)
	}
	if record.Status == "in-use" && record.InstanceId != instance.ID {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidNetworkInterfaceInUse)
	}

	attachmentID, err := d.vpcService.AttachENI(accountID, eniID, instance.ID, deviceIndex)
	if err != nil {
		return respondServiceErrorOutcome(d.node, msg, err)
	}
	if err := d.vpcService.UpdateENI(accountID, eniID, func(r *handlers_ec2_vpc.ENIRecord) {
		r.AttachmentStatus = "attaching"
		r.AttachmentStateAt = time.Now()
		r.LastAttachError = ""
	}); err != nil {
		slog.WarnContext(ctx, "AttachNetworkInterface: failed to mark attaching state",
			"eniId", eniID, "err", err)
	}

	res, hotPlugErr := d.vmMgr.HotPlugENI(ctx, instance, eniID, record.MacAddress)
	if hotPlugErr != nil {
		slog.ErrorContext(ctx, "AttachNetworkInterface: hot-plug pipeline failed, rolling back KV",
			"eniId", eniID, "instanceId", instance.ID, "err", hotPlugErr)
		if rollbackErr := d.vpcService.DetachENI(ctx, accountID, eniID); rollbackErr != nil {
			slog.ErrorContext(ctx, "AttachNetworkInterface: KV rollback failed",
				"eniId", eniID, "err", rollbackErr)
		}
		_ = d.vpcService.UpdateENI(accountID, eniID, func(r *handlers_ec2_vpc.ENIRecord) {
			r.AttachmentStatus = ""
			r.HotPlugSlot = 0
			r.LastAttachError = hotPlugErr.Error()
		})
		return respondErrorOutcome(d.node, msg, eniHotplugErrorCode(hotPlugErr))
	}

	if err := d.vpcService.UpdateENI(accountID, eniID, func(r *handlers_ec2_vpc.ENIRecord) {
		r.AttachmentStatus = "attached"
		r.HotPlugSlot = res.Slot
		r.AttachmentStateAt = time.Now()
	}); err != nil {
		slog.WarnContext(ctx, "AttachNetworkInterface: failed to mark attached state",
			"eniId", eniID, "err", err)
	}

	publishENIHotplugEvent(d.natsConn, "vpc.eni-hotplug.attached", instance.ID, map[string]any{
		"eniId":        eniID,
		"mac":          record.MacAddress,
		"attachmentId": attachmentID,
		"hotPlugSlot":  res.Slot,
		"deviceIndex":  deviceIndex,
	})

	respondWithJSON(d.node, msg, ec2.AttachNetworkInterfaceOutput{
		AttachmentId: aws.String(attachmentID),
	})
	return outcomeSuccess
}

// handleDetachNetworkInterface marks the KV record as detaching first
// (crash-safe), runs the QMP hot-unplug pipeline, then returns the
// record to available on success.
func (d *Daemon) handleDetachNetworkInterface(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) string {
	slog.InfoContext(ctx, "Detaching ENI from instance", "instanceId", command.ID)

	if command.DetachENIData == nil || command.DetachENIData.AttachmentID == "" {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidParameterValue)
	}

	attachmentID := command.DetachENIData.AttachmentID
	force := command.DetachENIData.Force
	accountID := utils.AccountIDFromMsg(msg)

	record, err := d.vpcService.FindENIByAttachment(accountID, attachmentID)
	if err != nil {
		return respondServiceErrorOutcome(d.node, msg, err)
	}
	if record.InstanceId != instance.ID {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidAttachmentIDNotFound)
	}

	if status := d.vmMgr.Status(instance); status != vm.StateRunning {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorIncorrectInstanceState)
	}

	if err := d.vpcService.UpdateENI(accountID, record.NetworkInterfaceId, func(r *handlers_ec2_vpc.ENIRecord) {
		r.AttachmentStatus = "detaching"
		r.AttachmentStateAt = time.Now()
		r.DetachInFlight = true
		r.DetachForce = force
	}); err != nil {
		slog.WarnContext(ctx, "DetachNetworkInterface: failed to mark detaching state",
			"eniId", record.NetworkInterfaceId, "err", err)
	}

	if err := d.vmMgr.HotUnplugENI(ctx, instance, record.NetworkInterfaceId, force); err != nil {
		slog.ErrorContext(ctx, "DetachNetworkInterface: hot-unplug pipeline failed",
			"eniId", record.NetworkInterfaceId, "instanceId", instance.ID, "err", err)
		_ = d.vpcService.UpdateENI(accountID, record.NetworkInterfaceId, func(r *handlers_ec2_vpc.ENIRecord) {
			r.DetachInFlight = false
		})
		return respondErrorOutcome(d.node, msg, eniHotplugErrorCode(err))
	}

	if err := d.vpcService.DetachENI(ctx, accountID, record.NetworkInterfaceId); err != nil {
		slog.WarnContext(ctx, "DetachNetworkInterface: KV detach failed after QMP success",
			"eniId", record.NetworkInterfaceId, "err", err)
	}
	_ = d.vpcService.UpdateENI(accountID, record.NetworkInterfaceId, func(r *handlers_ec2_vpc.ENIRecord) {
		r.AttachmentStatus = ""
		r.HotPlugSlot = 0
		r.DetachInFlight = false
		r.DetachForce = false
		r.AttachmentStateAt = time.Now()
	})

	publishENIHotplugEvent(d.natsConn, "vpc.eni-hotplug.detached", instance.ID, map[string]any{
		"eniId":        record.NetworkInterfaceId,
		"mac":          record.MacAddress,
		"attachmentId": attachmentID,
	})

	respondWithJSON(d.node, msg, ec2.DetachNetworkInterfaceOutput{})
	return outcomeSuccess
}

// eniHotplugErrorCode maps a vm.Manager hot-plug error to the AWS API
// error code that the AWS SDK surfaces.
func eniHotplugErrorCode(err error) string {
	switch {
	case errors.Is(err, vm.ErrInstanceNotFound):
		return awserrors.ErrorInvalidInstanceIDNotFound
	case errors.Is(err, vm.ErrInvalidTransition):
		return awserrors.ErrorIncorrectInstanceState
	case errors.Is(err, vm.ErrAttachmentLimitExceeded):
		return awserrors.ErrorAttachmentLimitExceeded
	case errors.Is(err, vm.ErrENINotAttached):
		return awserrors.ErrorInvalidAttachmentIDNotFound
	case errors.Is(err, vm.ErrQMPUnavailable),
		errors.Is(err, vm.ErrENIPipelineTimeout):
		return awserrors.ErrorServerInternal
	default:
		return awserrors.ErrorServerInternal
	}
}

// publishENIHotplugEvent emits the per-instance NATS event on best-effort
// basis — observers such as the ecs-agent and future vpcd subscribers read it
// for fanout. A publish failure does not roll back the pipeline.
func publishENIHotplugEvent(nc *nats.Conn, subject, instanceID string, payload map[string]any) {
	if nc == nil {
		return
	}
	fullSubject := subject + "." + instanceID
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("ENI hot-plug event marshal failed",
			"subject", fullSubject, "err", err)
		return
	}
	if err := nc.Publish(fullSubject, data); err != nil {
		slog.Warn("ENI hot-plug event publish failed",
			"subject", fullSubject, "err", err)
	}
}
