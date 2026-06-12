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

// terminateStoppedInstanceRequest is the payload sent to the ec2.terminate topic
type terminateStoppedInstanceRequest struct {
	InstanceID string `json:"instance_id"`
}

func ValidateTerminateInstancesInput(input *ec2.TerminateInstancesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.InstanceIds) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// TerminateInstances sends terminate commands via NATS with stop_instance set to prevent restart.
func TerminateInstances(input *ec2.TerminateInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.TerminateInstancesOutput, error) {
	if err := ValidateTerminateInstancesInput(input); err != nil {
		return nil, err
	}

	slog.Info("TerminateInstances: Processing request", "instance_count", len(input.InstanceIds))

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
				TerminateInstance: true,
			},
		}

		jsonData, err := json.Marshal(command)
		if err != nil {
			slog.Error("TerminateInstances: Failed to marshal command", "instance_id", instanceID, "err", err)
			continue
		}

		// Retry on ErrNoResponders: per-instance NATS subscription may not have propagated yet after a cluster restart.
		subject := fmt.Sprintf("ec2.cmd.%s", instanceID)
		var msg *nats.Msg
		for attempt := range 3 {
			reqMsg := nats.NewMsg(subject)
			reqMsg.Data = jsonData
			reqMsg.Header.Set(utils.AccountIDHeader, accountID)
			msg, err = natsConn.RequestMsg(reqMsg, 5*time.Second)
			if err == nil || !errors.Is(err, nats.ErrNoResponders) {
				break
			}
			if attempt < 2 {
				slog.Debug("TerminateInstances: No responder on per-instance topic, retrying",
					"instance_id", instanceID, "attempt", attempt+1)
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
		}
		if err != nil {
			// If no daemon owns this instance, try the ec2.terminate topic for stopped instances
			if errors.Is(err, nats.ErrNoResponders) {
				slog.Info("TerminateInstances: No responder on per-instance topic, trying ec2.terminate", "instance_id", instanceID)

				terminateReq, err := json.Marshal(terminateStoppedInstanceRequest{InstanceID: instanceID})
				if err != nil {
					slog.Error("TerminateInstances: Failed to marshal terminate request", "instance_id", instanceID, "err", err)
					continue
				}
				terminateReqMsg := nats.NewMsg("ec2.terminate")
				terminateReqMsg.Data = terminateReq
				terminateReqMsg.Header.Set(utils.AccountIDHeader, accountID)
				terminateMsg, terminateErr := natsConn.RequestMsg(terminateReqMsg, 30*time.Second)
				if terminateErr == nil {
					if _, parseErr := utils.ValidateErrorPayload(terminateMsg.Data); parseErr == nil {
						slog.Info("TerminateInstances: Stopped instance terminated via ec2.terminate", "instance_id", instanceID)
						stateChanges = append(stateChanges, newStateChange(instanceID, 32, "shutting-down", 80, "stopped"))
						continue
					}
				}

				// Check if instance is already terminated (idempotent, matches AWS behavior)
				if isAlreadyTerminated(natsConn, instanceID, accountID) {
					slog.Info("TerminateInstances: Instance already terminated", "instance_id", instanceID)
					stateChanges = append(stateChanges, newStateChange(instanceID, 48, "terminated", 48, "terminated"))
					continue
				}
			}

			slog.Error("TerminateInstances: Failed to send command", "instance_id", instanceID, "err", err)
			return nil, fmt.Errorf("failed to terminate instance %s: %w", instanceID, err)
		}

		if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
			slog.Error("TerminateInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
			return nil, errors.New(*responseError.Code)
		}

		slog.Info("TerminateInstances: Command sent successfully", "instance_id", instanceID, "response", string(msg.Data))

		stateChanges = append(stateChanges, newStateChange(instanceID, 32, "shutting-down", 16, "running"))
	}

	output := &ec2.TerminateInstancesOutput{
		TerminatingInstances: stateChanges,
	}

	slog.Info("TerminateInstances: Completed", "total_instances", len(stateChanges))
	return output, nil
}

// isAlreadyTerminated checks if an instance exists in the terminated KV bucket.
func isAlreadyTerminated(natsConn *nats.Conn, instanceID, accountID string) bool {
	describeInput := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{&instanceID},
	}
	reqData, err := json.Marshal(describeInput)
	if err != nil {
		slog.Warn("isAlreadyTerminated: failed to marshal request", "instanceId", instanceID, "err", err)
		return false
	}
	reqMsg := nats.NewMsg("ec2.DescribeTerminatedInstances")
	reqMsg.Data = reqData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 3*time.Second)
	if err != nil {
		slog.Warn("isAlreadyTerminated: failed to query terminated instances", "instanceId", instanceID, "err", err)
		return false
	}
	var output ec2.DescribeInstancesOutput
	if unmarshalErr := json.Unmarshal(msg.Data, &output); unmarshalErr != nil {
		slog.Warn("isAlreadyTerminated: failed to unmarshal response", "instanceId", instanceID, "err", unmarshalErr)
		return false
	}
	for _, res := range output.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceId != nil && *inst.InstanceId == instanceID {
				return true
			}
		}
	}
	return false
}
