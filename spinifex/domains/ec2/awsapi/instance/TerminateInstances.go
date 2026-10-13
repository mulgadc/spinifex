package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// terminateStoppedInstanceRequest is the payload sent to the ec2.terminate topic.
type terminateStoppedInstanceRequest struct {
	InstanceID string `json:"instance_id"`
}

// ValidateTerminateInstancesInput rejects a nil input with InvalidParameterValue and an empty
// InstanceIds list with MissingParameter, and a malformed instance ID with
// InvalidInstanceID.Malformed before any NATS traffic.
func ValidateTerminateInstancesInput(input *ec2.TerminateInstancesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.InstanceIds) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return validateInstanceIDFormat(input.InstanceIds)
}

// TerminateInstances sends terminate commands via NATS with stop_instance set to prevent restart.
func TerminateInstances(ctx context.Context, input *ec2.TerminateInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.TerminateInstancesOutput, error) {
	if err := ValidateTerminateInstancesInput(input); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "TerminateInstances: Processing request", "instance_count", len(input.InstanceIds))

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
				TerminateInstance: true,
			},
		}

		jsonData, err := json.Marshal(command)
		if err != nil {
			slog.ErrorContext(ctx, "TerminateInstances: Failed to marshal command", "instance_id", instanceID, "err", err)
			continue
		}

		msg, err := requestInstanceCmd(ctx, natsConn, instanceID, accountID, jsonData)
		if err != nil {
			// If no daemon owns this instance, try the ec2.terminate topic for stopped instances
			if errors.Is(err, nats.ErrNoResponders) {
				slog.InfoContext(ctx, "TerminateInstances: No responder on per-instance topic, trying ec2.terminate", "instance_id", instanceID)

				terminateReq, err := json.Marshal(terminateStoppedInstanceRequest{InstanceID: instanceID})
				if err != nil {
					slog.ErrorContext(ctx, "TerminateInstances: Failed to marshal terminate request", "instance_id", instanceID, "err", err)
					continue
				}
				terminateReqMsg := nats.NewMsg("ec2.terminate")
				terminateReqMsg.Data = terminateReq
				terminateReqMsg.Header.Set(natsmsg.AccountIDHeader, accountID)
				natsmsg.InjectTraceContext(ctx, terminateReqMsg.Header)
				terminateMsg, terminateErr := natsConn.RequestMsg(terminateReqMsg, 30*time.Second)
				daemonReportedAbsent := false
				if terminateErr == nil {
					responseError, parseErr := awserrors.ValidateErrorPayload(terminateMsg.Data)
					if parseErr == nil {
						slog.InfoContext(ctx, "TerminateInstances: Stopped instance terminated via ec2.terminate", "instance_id", instanceID)
						stateChanges = append(stateChanges, newStateChange(instanceID, 32, "shutting-down", 80, "stopped"))
						continue
					}

					// Only a genuine absence may fall through to the bucket check
					// below. Any other refusal is a terminate that did not happen,
					// so returning success for it strands a record the caller has
					// been told is gone.
					code := aws.StringValue(responseError.Code)
					if code != awserrors.ErrorInvalidInstanceIDNotFound {
						slog.ErrorContext(ctx, "TerminateInstances: ec2.terminate refused the instance",
							"instance_id", instanceID, "code", code)
						return nil, errors.New(code)
					}
					daemonReportedAbsent = true
					slog.InfoContext(ctx, "TerminateInstances: instance is not a stopped instance, checking the instance buckets",
						"instance_id", instanceID)
				}

				// An instance still in the terminated bucket returns 48/48 so repeat
				// terminates converge. NotFound needs the daemon's own NotFound plus
				// both buckets readable and empty; anything less cannot prove absence.
				state, queryOK := classifyUnownedInstance(ctx, natsConn, instanceID, accountID)
				if state == bucketStateTerminated {
					slog.InfoContext(ctx, "TerminateInstances: Instance already terminated", "instance_id", instanceID)
					stateChanges = append(stateChanges, newStateChange(instanceID, 48, "terminated", 48, "terminated"))
					continue
				}
				if queryOK && state == bucketStateAbsent && daemonReportedAbsent {
					slog.InfoContext(ctx, "TerminateInstances: Instance does not exist", "instance_id", instanceID)
					return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
				}
				slog.ErrorContext(ctx, "TerminateInstances: cannot confirm instance state",
					"instance_id", instanceID, "bucket_state", state, "query_ok", queryOK, "terminate_err", terminateErr)
			}

			slog.ErrorContext(ctx, "TerminateInstances: Failed to send command", "instance_id", instanceID, "err", err)
			return nil, fmt.Errorf("failed to terminate instance %s: %w", instanceID, err)
		}

		if responseError, parseErr := awserrors.ValidateErrorPayload(msg.Data); parseErr != nil {
			slog.ErrorContext(ctx, "TerminateInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
			return nil, errors.New(*responseError.Code)
		}

		slog.InfoContext(ctx, "TerminateInstances: Command sent successfully", "instance_id", instanceID, "response", string(msg.Data))

		stateChanges = append(stateChanges, newStateChange(instanceID, 32, "shutting-down", 16, "running"))
	}

	output := &ec2.TerminateInstancesOutput{
		TerminatingInstances: stateChanges,
	}

	slog.InfoContext(ctx, "TerminateInstances: Completed", "total_instances", len(stateChanges))
	return output, nil
}
