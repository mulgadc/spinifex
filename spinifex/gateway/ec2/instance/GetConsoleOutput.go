package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

func ValidateGetConsoleOutputInput(input *ec2.GetConsoleOutputInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.InstanceId == nil || *input.InstanceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// GetConsoleOutput retrieves console output for a specific instance via NATS.
// Routes directly to the node running the instance via ec2.{instanceID}.GetConsoleOutput.
func GetConsoleOutput(input *ec2.GetConsoleOutputInput, natsConn *nats.Conn, accountID string) (*ec2.GetConsoleOutputOutput, error) {
	if err := ValidateGetConsoleOutputInput(input); err != nil {
		return nil, err
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	topic := fmt.Sprintf("ec2.%s.GetConsoleOutput", *input.InstanceId)
	reqMsg := nats.NewMsg(topic)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 5*time.Second)
	if err != nil {
		// No daemon subscribed to the per-instance topic: stopped, terminated,
		// or non-existent — all of which AWS surfaces as InvalidInstanceID.NotFound.
		// Treat timeout the same way; a daemon that can't respond inside 5s
		// is functionally unreachable from a caller's perspective.
		if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, nats.ErrTimeout) {
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		return nil, fmt.Errorf("failed to get console output: %w", err)
	}

	responseError, parseErr := utils.ValidateErrorPayload(msg.Data)
	if parseErr != nil {
		slog.Error("GetConsoleOutput: Daemon returned error", "instance_id", *input.InstanceId, "code", *responseError.Code)
		return nil, errors.New(*responseError.Code)
	}

	var output ec2.GetConsoleOutputOutput
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.Error("GetConsoleOutput: Failed to unmarshal response", "err", err)
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &output, nil
}
