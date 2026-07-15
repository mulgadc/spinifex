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
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ValidateAttachVolumeInput validates the input parameters for AttachVolume.
func ValidateAttachVolumeInput(input *ec2.AttachVolumeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.VolumeId == nil || *input.VolumeId == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.InstanceId == nil || *input.InstanceId == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return nil
}

// AttachVolume sends an attach-volume command to the daemon owning the instance.
func AttachVolume(ctx context.Context, input *ec2.AttachVolumeInput, natsConn *nats.Conn, accountID string) (ec2.VolumeAttachment, error) {
	var output ec2.VolumeAttachment

	if err := ValidateAttachVolumeInput(input); err != nil {
		return output, err
	}

	instanceID := *input.InstanceId
	volumeID := *input.VolumeId

	device := ""
	if input.Device != nil {
		device = *input.Device
	}

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &types.AttachVolumeData{
			VolumeID: volumeID,
			Device:   device,
		},
	}

	jsonData, err := json.Marshal(command)
	if err != nil {
		slog.ErrorContext(ctx, "AttachVolume: Failed to marshal command", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	subject := fmt.Sprintf("ec2.cmd.%s", instanceID)
	reqMsg := nats.NewMsg(subject)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.ErrorContext(ctx, "AttachVolume: NATS request failed", "instanceId", instanceID, "volumeId", volumeID, "err", err)
		if errors.Is(err, nats.ErrNoResponders) {
			if isStoppedInstance(ctx, instanceID, natsConn, accountID) {
				return output, errors.New(awserrors.ErrorIncorrectInstanceState)
			}
			return output, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	responseError, err := utils.ValidateErrorPayload(msg.Data)
	if err != nil {
		return output, errors.New(*responseError.Code)
	}

	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.ErrorContext(ctx, "AttachVolume: Failed to unmarshal response", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "AttachVolume completed", "instanceId", instanceID, "volumeId", volumeID)
	return output, nil
}

// isStoppedInstance checks the shared KV (via ec2.DescribeStoppedInstances) to
// determine whether instanceID exists as a stopped instance.
func isStoppedInstance(ctx context.Context, instanceID string, natsConn *nats.Conn, accountID string) bool {
	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}
	reqData, err := json.Marshal(input)
	if err != nil {
		return false
	}

	reqMsg := nats.NewMsg("ec2.DescribeStoppedInstances")
	reqMsg.Data = reqData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 3*time.Second)
	if err != nil {
		return false
	}

	if _, err := utils.ValidateErrorPayload(msg.Data); err != nil {
		return false
	}

	var output ec2.DescribeInstancesOutput
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		return false
	}

	for _, res := range output.Reservations {
		if len(res.Instances) > 0 {
			return true
		}
	}
	return false
}
