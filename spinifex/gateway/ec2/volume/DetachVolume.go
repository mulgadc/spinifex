package gateway_ec2_volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ValidateDetachVolumeInput validates the input parameters for DetachVolume.
func ValidateDetachVolumeInput(input *ec2.DetachVolumeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.VolumeId == nil || *input.VolumeId == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return nil
}

// DetachVolume sends a detach-volume command to the daemon owning the instance.
func DetachVolume(ctx context.Context, input *ec2.DetachVolumeInput, natsConn *nats.Conn, accountID string) (ec2.VolumeAttachment, error) {
	var output ec2.VolumeAttachment

	if err := ValidateDetachVolumeInput(input); err != nil {
		return output, err
	}

	volumeID := *input.VolumeId

	var instanceID string
	if input.InstanceId != nil && *input.InstanceId != "" {
		instanceID = *input.InstanceId
	} else {
		volSvc := handlers_ec2_volume.NewNATSVolumeService(natsConn)
		descOutput, err := volSvc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []*string{&volumeID},
		}, accountID)
		if err != nil {
			slog.ErrorContext(ctx, "DetachVolume: failed to describe volume", "volumeId", volumeID, "err", err)
			return output, errors.New(awserrors.ErrorInvalidVolumeNotFound)
		}
		if len(descOutput.Volumes) == 0 {
			return output, errors.New(awserrors.ErrorInvalidVolumeNotFound)
		}
		vol := descOutput.Volumes[0]
		if len(vol.Attachments) == 0 || vol.Attachments[0].InstanceId == nil {
			return output, errors.New(awserrors.ErrorIncorrectState)
		}
		instanceID = *vol.Attachments[0].InstanceId
	}

	device := ""
	if input.Device != nil {
		device = *input.Device
	}

	force := input.Force != nil && *input.Force

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			DetachVolume: true,
		},
		DetachVolumeData: &types.DetachVolumeData{
			VolumeID: volumeID,
			Device:   device,
			Force:    force,
		},
	}

	jsonData, err := json.Marshal(command)
	if err != nil {
		slog.ErrorContext(ctx, "DetachVolume: Failed to marshal command", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	subject := fmt.Sprintf("ec2.cmd.%s", instanceID)
	reqMsg := nats.NewMsg(subject)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.ErrorContext(ctx, "DetachVolume: NATS request failed", "instanceId", instanceID, "volumeId", volumeID, "err", err)
		if errors.Is(err, nats.ErrNoResponders) {
			if force {
				return forceDetach(ctx, natsConn, accountID, volumeID, instanceID)
			}
			return output, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	responseError, err := utils.ValidateErrorPayload(msg.Data)
	if err != nil {
		if responseError.Code != nil {
			return output, errors.New(*responseError.Code)
		}
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.ErrorContext(ctx, "DetachVolume: Failed to unmarshal response", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DetachVolume completed", "instanceId", instanceID, "volumeId", volumeID)
	return output, nil
}

// forceDetachSubject clears a volume's attachment in the control plane only and
// is answered by any node, unlike the ordinary detach, which routes to the host
// running the instance and so cannot help when that host is the problem.
const forceDetachSubject = "ec2.ForceDetachVolume"

// forceDetach releases a volume whose instance has no responder.
//
// A volume outlives the instance it was attached to whenever teardown failed
// part-way, and the ordinary detach then has nowhere to go: there is no guest
// to unplug it from and no host to ask. Without this the volume is attached
// forever, which also makes it undeletable. Reaching here needs Force, which is
// the caller stating that no clean guest unmount is expected.
func forceDetach(ctx context.Context, natsConn *nats.Conn, accountID, volumeID, instanceID string) (ec2.VolumeAttachment, error) {
	attachment, err := utils.NATSRequest[ec2.VolumeAttachment](ctx, natsConn, forceDetachSubject,
		&ec2.DetachVolumeInput{VolumeId: &volumeID, Force: aws.Bool(true)}, 30*time.Second, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "DetachVolume: force detach failed", "volumeId", volumeID, "instanceId", instanceID, "err", err)
		return ec2.VolumeAttachment{}, err
	}

	slog.WarnContext(ctx, "DetachVolume: forced, the instance had no responder",
		"volumeId", volumeID, "instanceId", instanceID)
	if attachment == nil {
		return ec2.VolumeAttachment{}, nil
	}
	return *attachment, nil
}
