package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

func ValidateRebootInstancesInput(input *ec2.RebootInstancesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.InstanceIds) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// RebootInstances sends reboot commands to specified instances via NATS.
// Unlike stop+start, reboot keeps the instance running and sends a QMP system_reset.
// Returns an empty response on success (AWS returns no state-change data).
func RebootInstances(input *ec2.RebootInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.RebootInstancesOutput, error) {
	if err := ValidateRebootInstancesInput(input); err != nil {
		return nil, err
	}

	slog.Info("RebootInstances: Processing request", "instance_count", len(input.InstanceIds))

	for _, instanceIDPtr := range input.InstanceIds {
		if instanceIDPtr == nil {
			continue
		}
		instanceID := *instanceIDPtr

		command := types.EC2InstanceCommand{
			ID: instanceID,
			Attributes: types.EC2CommandAttributes{
				RebootInstance: true,
			},
		}

		jsonData, err := json.Marshal(command)
		if err != nil {
			slog.Error("RebootInstances: Failed to marshal command", "instance_id", instanceID, "err", err)
			continue
		}

		subject := fmt.Sprintf("ec2.cmd.%s", instanceID)
		reqMsg := nats.NewMsg(subject)
		reqMsg.Data = jsonData
		reqMsg.Header.Set(utils.AccountIDHeader, accountID)
		msg, err := natsConn.RequestMsg(reqMsg, 5*time.Second)
		if err != nil {
			slog.Error("RebootInstances: Failed to send command", "instance_id", instanceID, "err", err)

			// No daemon subscription: check stopped-KV to return IncorrectInstanceState instead of NotFound.
			describeInput := &ec2.DescribeInstancesInput{
				InstanceIds: []*string{&instanceID},
			}
			describeData, err := json.Marshal(describeInput)
			if err != nil {
				return nil, fmt.Errorf("marshal describe input: %w", err)
			}
			if reservations, _ := queryInstanceBucket(natsConn, "ec2.DescribeStoppedInstances", describeData, accountID); len(reservations) > 0 {
				return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
			}

			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}

		if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
			slog.Error("RebootInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
			return nil, errors.New(*responseError.Code)
		}

		slog.Info("RebootInstances: Command sent successfully", "instance_id", instanceID)
	}

	slog.Info("RebootInstances: Completed", "total_instances", len(input.InstanceIds))
	return &ec2.RebootInstancesOutput{}, nil
}
