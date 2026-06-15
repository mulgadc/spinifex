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

func ValidateStopInstancesInput(input *ec2.StopInstancesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.InstanceIds) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// StopInstances sends stop commands via NATS using system_powerdown with stop_instance
// set to prevent auto-restart on daemon boot.
func StopInstances(input *ec2.StopInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.StopInstancesOutput, error) {
	if err := ValidateStopInstancesInput(input); err != nil {
		return nil, err
	}

	slog.Info("StopInstances: Processing request", "instance_count", len(input.InstanceIds))

	var stateChanges []*ec2.InstanceStateChange

	for _, instanceIDPtr := range input.InstanceIds {
		if instanceIDPtr == nil {
			continue
		}
		instanceID := *instanceIDPtr

		command := types.EC2InstanceCommand{
			ID: instanceID,
			Attributes: types.EC2CommandAttributes{
				StopInstance:      true,
				TerminateInstance: false,
			},
		}

		jsonData, err := json.Marshal(command)
		if err != nil {
			slog.Error("StopInstances: Failed to marshal command", "instance_id", instanceID, "err", err)
			continue
		}

		subject := fmt.Sprintf("ec2.cmd.%s", instanceID)
		reqMsg := nats.NewMsg(subject)
		reqMsg.Data = jsonData
		reqMsg.Header.Set(utils.AccountIDHeader, accountID)
		msg, err := natsConn.RequestMsg(reqMsg, 5*time.Second)
		if err != nil {
			slog.Error("StopInstances: Failed to send command", "instance_id", instanceID, "err", err)
			stateChanges = append(stateChanges, newStateChange(instanceID, 16, "running", 16, "running"))
			continue
		}

		if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
			slog.Error("StopInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
			return nil, errors.New(*responseError.Code)
		}

		slog.Info("StopInstances: Command sent successfully", "instance_id", instanceID, "response", string(msg.Data))

		stateChanges = append(stateChanges, newStateChange(instanceID, 64, "stopping", 16, "running"))
	}

	output := &ec2.StopInstancesOutput{
		StoppingInstances: stateChanges,
	}

	slog.Info("StopInstances: Completed", "total_instances", len(stateChanges))
	return output, nil
}
