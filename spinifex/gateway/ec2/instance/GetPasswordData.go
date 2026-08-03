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
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// getPasswordDataTimeout is the NATS request timeout, overridable in tests so
// the ErrTimeout path can be exercised without a real 5-second wait.
var getPasswordDataTimeout = 5 * time.Second

func ValidateGetPasswordDataInput(input *ec2.GetPasswordDataInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.InstanceId == nil || *input.InstanceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// GetPasswordData retrieves the Windows administrator password ciphertext
// for a specific instance via NATS. Routes directly to the node running the
// instance via ec2.{instanceID}.GetPasswordData.
func GetPasswordData(ctx context.Context, input *ec2.GetPasswordDataInput, natsConn *nats.Conn, accountID string) (*ec2.GetPasswordDataOutput, error) {
	if err := ValidateGetPasswordDataInput(input); err != nil {
		return nil, err
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	topic := fmt.Sprintf("ec2.%s.GetPasswordData", *input.InstanceId)
	reqMsg := nats.NewMsg(topic)
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, getPasswordDataTimeout)
	if err != nil {
		// No daemon subscription or timeout: stopped/terminated/non-existent instances all surface as NotFound.
		if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, nats.ErrTimeout) {
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		return nil, fmt.Errorf("failed to get password data: %w", err)
	}

	responseError, parseErr := utils.ValidateErrorPayload(msg.Data)
	if parseErr != nil {
		slog.ErrorContext(ctx, "GetPasswordData: Daemon returned error", "instance_id", *input.InstanceId, "code", *responseError.Code)
		return nil, errors.New(*responseError.Code)
	}

	var output ec2.GetPasswordDataOutput
	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.ErrorContext(ctx, "GetPasswordData: Failed to unmarshal response", "err", err)
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &output, nil
}
