package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateModifyInstanceAttributeInput validates the input constraints for ModifyInstanceAttribute.
// AWS answers InvalidParameterCombination when no attribute, or more than one,
// is set in a single request.
func ValidateModifyInstanceAttributeInput(input *ec2.ModifyInstanceAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.InstanceId == nil || *input.InstanceId == "" {
		return errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}
	if !strings.HasPrefix(*input.InstanceId, "i-") {
		return errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}

	count := 0
	if input.InstanceType != nil {
		count++
	}
	if input.UserData != nil {
		count++
	}
	if input.SourceDestCheck != nil {
		count++
	}
	if input.DisableApiTermination != nil {
		count++
	}
	switch {
	case count == 0:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterCombination, "No attributes specified.")
	case count > 1:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterCombination, "The request must contain a single attribute")
	}

	if input.InstanceType != nil && (input.InstanceType.Value == nil || *input.InstanceType.Value == "") {
		return errors.New(awserrors.ErrorInvalidInstanceAttributeValue)
	}

	return nil
}

// ModifyInstanceAttribute sends a modify request to the daemon via NATS.
// The daemon updates the stopped instance in KV and returns an empty response on success.
func ModifyInstanceAttribute(ctx context.Context, input *ec2.ModifyInstanceAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ModifyInstanceAttributeOutput, error) {
	if err := ValidateModifyInstanceAttributeInput(input); err != nil {
		return ec2.ModifyInstanceAttributeOutput{}, err
	}

	slog.InfoContext(ctx, "ModifyInstanceAttribute: Processing request", "instance_id", *input.InstanceId)

	// Termination protection on a running instance lives on its owner, so it
	// goes there first. A nil value is not a value and keeps the queue path.
	if input.DisableApiTermination != nil && input.DisableApiTermination.Value != nil {
		cmd := ec2v1.EC2InstanceCommand{
			ID:                        *input.InstanceId,
			Attributes:                ec2v1.EC2CommandAttributes{SetDisableApiTermination: true},
			DisableApiTerminationData: &ec2v1.DisableApiTerminationData{Value: *input.DisableApiTermination.Value},
		}
		if _, err := routeInstanceMutation[ec2.ModifyInstanceAttributeOutput](ctx, natsConn, *input.InstanceId, accountID, cmd, "ec2.ModifyInstanceAttribute", input); err != nil {
			return ec2.ModifyInstanceAttributeOutput{}, err
		}
		return ec2.ModifyInstanceAttributeOutput{}, nil
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.ErrorContext(ctx, "ModifyInstanceAttribute: Failed to marshal request", "instance_id", *input.InstanceId, "err", err)
		return ec2.ModifyInstanceAttributeOutput{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	reqMsg := nats.NewMsg("ec2.ModifyInstanceAttribute")
	reqMsg.Data = jsonData
	reqMsg.Header.Set(natsmsg.AccountIDHeader, accountID)
	natsmsg.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.ErrorContext(ctx, "ModifyInstanceAttribute: Failed to send request", "instance_id", *input.InstanceId, "err", err)
		return ec2.ModifyInstanceAttributeOutput{}, fmt.Errorf("failed to send modify request: %w", err)
	}

	if responseError, parseErr := awserrors.ValidateErrorPayload(msg.Data); parseErr != nil {
		slog.ErrorContext(ctx, "ModifyInstanceAttribute: Daemon returned error", "instance_id", *input.InstanceId, "code", *responseError.Code)
		return ec2.ModifyInstanceAttributeOutput{}, errors.New(*responseError.Code)
	}

	slog.InfoContext(ctx, "ModifyInstanceAttribute: Completed successfully", "instance_id", *input.InstanceId)
	return ec2.ModifyInstanceAttributeOutput{}, nil
}
