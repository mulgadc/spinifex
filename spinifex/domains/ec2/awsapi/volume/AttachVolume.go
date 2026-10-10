package volume

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

	command := ec2v1.EC2InstanceCommand{
		ID: instanceID,
		Attributes: ec2v1.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &ec2v1.AttachVolumeData{
			VolumeID: volumeID,
			Device:   device,
		},
	}

	jsonData, err := json.Marshal(command)
	if err != nil {
		slog.ErrorContext(ctx, "AttachVolume: Failed to marshal command", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	subject := ec2v1.InstanceCommandSubject(instanceID)
	reqMsg := nats.NewMsg(subject)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(natsmsg.AccountIDHeader, accountID)
	natsmsg.InjectTraceContext(ctx, reqMsg.Header)
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

	responseError, err := awserrors.ValidateErrorPayload(msg.Data)
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
	reqMsg.Header.Set(natsmsg.AccountIDHeader, accountID)
	natsmsg.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 3*time.Second)
	if err != nil {
		return false
	}

	if _, err := awserrors.ValidateErrorPayload(msg.Data); err != nil {
		return false
	}

	var output ec2.DescribeInstancesOutput
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		return false
	}

	return slices.ContainsFunc(output.Reservations, func(res *ec2.Reservation) bool {
		return len(res.Instances) > 0
	})
}
