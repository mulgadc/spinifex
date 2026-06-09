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

// startStoppedInstanceRequest is the payload sent to the ec2.start topic
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

// StartInstances starts the requested instances.
//
// A crash/recovery-failed instance stays live in its owner node's vmMgr as
// StateError, with its per-instance ec2.cmd subscription still active — so it
// is restarted via the owner-targeted ec2.cmd path. A genuinely stopped
// instance lives only in the shared stopped-KV with no live owner; for it the
// ec2.cmd attempt returns ErrNoResponders and we fall back to the ec2.start
// queue-group path that rehydrates from KV.
func StartInstances(input *ec2.StartInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.StartInstancesOutput, error) {
	if err := ValidateStartInstancesInput(input); err != nil {
		return nil, err
	}

	slog.Info("StartInstances: Processing request", "instance_count", len(input.InstanceIds))

	var stateChanges []*ec2.InstanceStateChange

	for _, instanceIDPtr := range input.InstanceIds {
		if instanceIDPtr == nil {
			continue
		}
		instanceID := *instanceIDPtr

		sc, handled, err := startLiveInstance(natsConn, instanceID, accountID)
		if err != nil {
			return nil, err
		}
		if handled {
			stateChanges = append(stateChanges, sc)
			continue
		}

		sc, err = startStoppedInstance(natsConn, instanceID, accountID)
		if err != nil {
			return nil, err
		}
		stateChanges = append(stateChanges, sc)
	}

	output := &ec2.StartInstancesOutput{
		StartingInstances: stateChanges,
	}

	slog.Info("StartInstances: Completed", "total_instances", len(stateChanges))
	return output, nil
}

// startLiveInstance restarts an instance still live on its owner node (e.g.
// StateError after exhausted auto-restart) via the per-instance ec2.cmd topic,
// reusing the StartInstance handler. handled is false with a nil error when no
// live owner is subscribed (ErrNoResponders), signalling the caller to fall
// back to the stopped-KV path.
func startLiveInstance(natsConn *nats.Conn, instanceID, accountID string) (*ec2.InstanceStateChange, bool, error) {
	command := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{StartInstance: true},
	}
	jsonData, err := json.Marshal(command)
	if err != nil {
		slog.Error("StartInstances: Failed to marshal cmd", "instance_id", instanceID, "err", err)
		return nil, false, errors.New(awserrors.ErrorServerInternal)
	}

	reqMsg := nats.NewMsg(fmt.Sprintf("ec2.cmd.%s", instanceID))
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		// No live owner subscribed — this is a genuinely stopped instance; let
		// the caller fall back to the stopped-KV start path.
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, false, nil
		}
		slog.Error("StartInstances: cmd request failed", "instance_id", instanceID, "err", err)
		return nil, false, errors.New(awserrors.ErrorServerInternal)
	}

	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.Error("StartInstances: owner returned error", "instance_id", instanceID, "code", *responseError.Code)
		return nil, false, errors.New(*responseError.Code)
	}

	slog.Info("StartInstances: restarted via owner node", "instance_id", instanceID, "response", string(msg.Data))
	return newStateChange(instanceID, 0, "pending", 80, "stopped"), true, nil
}

// startStoppedInstance rehydrates a stopped instance from the shared KV via the
// ec2.start queue-group topic. Any available daemon forwards to the instance's
// original node.
func startStoppedInstance(natsConn *nats.Conn, instanceID, accountID string) (*ec2.InstanceStateChange, error) {
	req := startStoppedInstanceRequest{InstanceID: instanceID}
	jsonData, err := json.Marshal(req)
	if err != nil {
		slog.Error("StartInstances: Failed to marshal request", "instance_id", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("StartInstances: Sending NATS request", "subject", "ec2.start", "instance_id", instanceID)

	reqMsg := nats.NewMsg("ec2.start")
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.Error("StartInstances: Failed to send start request", "instance_id", instanceID, "err", err)
		return newStateChange(instanceID, 80, "stopped", 80, "stopped"), nil
	}

	// Check if the daemon returned an error response
	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.Error("StartInstances: Daemon returned error", "instance_id", instanceID, "code", *responseError.Code)
		return nil, errors.New(*responseError.Code)
	}

	slog.Info("StartInstances: Command sent successfully", "instance_id", instanceID, "response", string(msg.Data))
	return newStateChange(instanceID, 0, "pending", 80, "stopped"), nil
}
