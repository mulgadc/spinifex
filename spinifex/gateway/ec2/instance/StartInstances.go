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

// startStoppedInstanceRequest is the payload sent to the ec2.start topic.
type startStoppedInstanceRequest struct {
	InstanceID string `json:"instance_id"`
}

func ValidateStartInstancesInput(input *ec2.StartInstancesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.InstanceIds) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// StartInstances starts the requested instances via the per-instance ec2.cmd topic,
// falling back to the ec2.start queue-group path (KV rehydration) on ErrNoResponders.
func StartInstances(ctx context.Context, input *ec2.StartInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.StartInstancesOutput, error) {
	if err := ValidateStartInstancesInput(input); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "StartInstances: Processing request", "instance_count", len(input.InstanceIds))

	var stateChanges []*ec2.InstanceStateChange

	for _, instanceIDPtr := range input.InstanceIds {
		if instanceIDPtr == nil {
			continue
		}
		instanceID := *instanceIDPtr

		sc, handled, err := startLiveInstance(ctx, natsConn, instanceID, accountID)
		if err != nil {
			return nil, err
		}
		if handled {
			stateChanges = append(stateChanges, sc)
			continue
		}

		sc, err = startStoppedInstance(ctx, natsConn, instanceID, accountID)
		if err != nil {
			return nil, err
		}
		stateChanges = append(stateChanges, sc)
	}

	output := &ec2.StartInstancesOutput{
		StartingInstances: stateChanges,
	}

	slog.InfoContext(ctx, "StartInstances: Completed", "total_instances", len(stateChanges))
	return output, nil
}

// startLiveInstance sends StartInstance via ec2.cmd.{id}. Returns handled=false on
// ErrNoResponders so the caller can fall back to the stopped-KV path.
func startLiveInstance(ctx context.Context, natsConn *nats.Conn, instanceID, accountID string) (*ec2.InstanceStateChange, bool, error) {
	command := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{StartInstance: true},
	}
	jsonData, err := json.Marshal(command)
	if err != nil {
		slog.ErrorContext(ctx, "StartInstances: Failed to marshal cmd", "instance_id", instanceID, "err", err)
		return nil, false, errors.New(awserrors.ErrorServerInternal)
	}

	reqMsg := nats.NewMsg(fmt.Sprintf("ec2.cmd.%s", instanceID))
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		// No live owner subscribed: fall back to the stopped-KV start path.
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, false, nil
		}
		slog.ErrorContext(ctx, "StartInstances: cmd request failed", "instance_id", instanceID, "err", err)
		return nil, false, errors.New(awserrors.ErrorServerInternal)
	}

	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.ErrorContext(ctx, "StartInstances: owner returned error", "instance_id", instanceID, "code", *responseError.Code)
		return nil, false, errors.New(*responseError.Code)
	}

	slog.InfoContext(ctx, "StartInstances: restarted via owner node", "instance_id", instanceID, "response", string(msg.Data))
	return newStateChange(instanceID, 0, "pending", 80, "stopped"), true, nil
}

// startStoppedInstance rehydrates a stopped instance from the shared KV via the
// ec2.start queue-group topic. Any available daemon forwards to the instance's
// original node.
func startStoppedInstance(ctx context.Context, natsConn *nats.Conn, instanceID, accountID string) (*ec2.InstanceStateChange, error) {
	req := startStoppedInstanceRequest{InstanceID: instanceID}
	jsonData, err := json.Marshal(req)
	if err != nil {
		slog.ErrorContext(ctx, "StartInstances: Failed to marshal request", "instance_id", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "StartInstances: Sending NATS request", "subject", "ec2.start", "instance_id", instanceID)

	reqMsg := nats.NewMsg("ec2.start")
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.ErrorContext(ctx, "StartInstances: Failed to send start request", "instance_id", instanceID, "err", err)
		return newStateChange(instanceID, 80, "stopped", 80, "stopped"), nil
	}

	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.ErrorContext(ctx, "StartInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
		return nil, errors.New(*responseError.Code)
	}

	slog.InfoContext(ctx, "StartInstances: Command sent successfully", "instance_id", instanceID, "response", string(msg.Data))
	return newStateChange(instanceID, 0, "pending", 80, "stopped"), nil
}
