package gateway_ec2_instance

import (
	"context"
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

// rebootInstancesTimeout is the NATS request timeout, overridable in tests so
// the ErrTimeout path can be exercised without a real 5-second wait. The daemon
// admits a reboot and runs it in the background, so this bounds the admission
// and never the guest's shutdown.
var rebootInstancesTimeout = 5 * time.Second

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
func RebootInstances(ctx context.Context, input *ec2.RebootInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.RebootInstancesOutput, error) {
	if err := ValidateRebootInstancesInput(input); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "RebootInstances: Processing request", "instance_count", len(input.InstanceIds))

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
			slog.ErrorContext(ctx, "RebootInstances: Failed to marshal command", "instance_id", instanceID, "err", err)
			continue
		}

		subject := fmt.Sprintf("ec2.cmd.%s", instanceID)
		reqMsg := nats.NewMsg(subject)
		reqMsg.Data = jsonData
		reqMsg.Header.Set(utils.AccountIDHeader, accountID)
		utils.InjectTraceContext(ctx, reqMsg.Header)
		msg, err := natsConn.RequestMsg(reqMsg, rebootInstancesTimeout)
		if err != nil {
			slog.ErrorContext(ctx, "RebootInstances: Failed to send command", "instance_id", instanceID, "err", err)

			// Only the absence of a subscriber says anything about where the
			// instance is. A timeout is a daemon that is slow or wedged, and
			// calling that NotFound sends the caller after an entirely
			// different fault than the one it hit.
			if !errors.Is(err, nats.ErrNoResponders) {
				return nil, errors.New(awserrors.ErrorServerInternal)
			}

			// No daemon subscription: check stopped-KV to return IncorrectInstanceState instead of NotFound.
			describeInput := &ec2.DescribeInstancesInput{
				InstanceIds: []*string{&instanceID},
			}
			describeData, err := json.Marshal(describeInput)
			if err != nil {
				return nil, fmt.Errorf("marshal describe input: %w", err)
			}
			if reservations, _ := queryInstanceBucket(ctx, natsConn, "ec2.DescribeStoppedInstances", describeData, accountID); len(reservations) > 0 {
				return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
			}

			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}

		if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
			slog.ErrorContext(ctx, "RebootInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
			return nil, errors.New(*responseError.Code)
		}

		slog.InfoContext(ctx, "RebootInstances: Command sent successfully", "instance_id", instanceID)
	}

	slog.InfoContext(ctx, "RebootInstances: Completed", "total_instances", len(input.InstanceIds))
	return &ec2.RebootInstancesOutput{}, nil
}
