package gateway_ec2_capacityreservation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// CancelCapacityReservation broadcasts the cancel to every node; only the daemon
// that owns the reservation releases its carve-out, but all daemons ack so the
// gateway can tell "cancelled" from "no node owns this id". Any ack with Return
// true means success; none means the id is unknown.
func CancelCapacityReservation(ctx context.Context, input *ec2.CancelCapacityReservationInput, natsConn *nats.Conn, expectedNodes int, accountID string) (ec2.CancelCapacityReservationOutput, error) {
	var output ec2.CancelCapacityReservationOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	id := aws.StringValue(input.CapacityReservationId)
	if id == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	if !strings.HasPrefix(id, "cr-") {
		return output, errors.New(awserrors.ErrorInvalidCapacityReservationIdMalformed)
	}
	if aws.BoolValue(input.DryRun) {
		return output, errors.New(awserrors.ErrorDryRunOperation)
	}

	payload, err := json.Marshal(input)
	if err != nil {
		return output, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, _, err := utils.GatherCtx(ctx, natsConn, "ec2.CancelCapacityReservation", payload,
		utils.GatherOpts{Timeout: censusTimeout, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return output, err
	}

	for _, frame := range frames {
		var ack ec2.CancelCapacityReservationOutput
		if json.Unmarshal(frame, &ack) == nil && aws.BoolValue(ack.Return) {
			output.Return = aws.Bool(true)
			return output, nil
		}
	}

	return output, errors.New(awserrors.ErrorInvalidCapacityReservationIdNotFound)
}
