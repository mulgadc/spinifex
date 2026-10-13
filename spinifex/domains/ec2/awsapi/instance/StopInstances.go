package instance

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateStopInstancesInput rejects a nil input with InvalidParameterValue and an empty
// InstanceIds list with MissingParameter, and a malformed instance ID with
// InvalidInstanceID.Malformed before any NATS traffic.
func ValidateStopInstancesInput(input *ec2.StopInstancesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.InstanceIds) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return validateInstanceIDFormat(input.InstanceIds)
}

// StopInstances sends stop commands via NATS using system_powerdown with stop_instance
// set to prevent auto-restart on daemon boot.
func StopInstances(ctx context.Context, input *ec2.StopInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.StopInstancesOutput, error) {
	if err := ValidateStopInstancesInput(input); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "StopInstances: Processing request", "instance_count", len(input.InstanceIds))

	var stateChanges []*ec2.InstanceStateChange

	for _, instanceIDPtr := range input.InstanceIds {
		if instanceIDPtr == nil {
			continue
		}
		instanceID := *instanceIDPtr

		command := ec2v1.EC2InstanceCommand{
			ID: instanceID,
			Attributes: ec2v1.EC2CommandAttributes{
				StopInstance:      true,
				TerminateInstance: false,
			},
		}

		jsonData, err := json.Marshal(command)
		if err != nil {
			slog.ErrorContext(ctx, "StopInstances: Failed to marshal command", "instance_id", instanceID, "err", err)
			continue
		}

		msg, err := requestInstanceCmd(ctx, natsConn, instanceID, accountID, jsonData)
		if err != nil {
			slog.ErrorContext(ctx, "StopInstances: Failed to send command", "instance_id", instanceID, "err", err)
			// A timeout is a slow or wedged owner, which says nothing about
			// where the instance is.
			if !errors.Is(err, nats.ErrNoResponders) {
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			state, queryOK := classifyUnownedInstance(ctx, natsConn, instanceID, accountID)
			switch {
			case state == bucketStateStopped:
				// Stopping a stopped instance succeeds as a no-op, as on AWS.
				stateChanges = append(stateChanges, newStateChange(instanceID, 80, "stopped", 80, "stopped"))
				continue
			case state == bucketStateTerminated:
				return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
			case queryOK:
				return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
			default:
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
		}

		if responseError, parseErr := awserrors.ValidateErrorPayload(msg.Data); parseErr != nil {
			slog.ErrorContext(ctx, "StopInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
			return nil, errors.New(*responseError.Code)
		}

		slog.InfoContext(ctx, "StopInstances: Command sent successfully", "instance_id", instanceID, "response", string(msg.Data))

		stateChanges = append(stateChanges, newStateChange(instanceID, 64, "stopping", 16, "running"))
	}

	output := &ec2.StopInstancesOutput{
		StoppingInstances: stateChanges,
	}

	slog.InfoContext(ctx, "StopInstances: Completed", "total_instances", len(stateChanges))
	return output, nil
}
