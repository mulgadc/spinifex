package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

func ValidateLBAgentHeartbeatInput(input *handlers_elbv2.LBAgentHeartbeatInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LBID == nil || *input.LBID == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// LBAgentHeartbeat handles the ELBv2 LBAgentHeartbeat API call.
func LBAgentHeartbeat(ctx context.Context, input *handlers_elbv2.LBAgentHeartbeatInput, natsConn *nats.Conn, accountID string) (handlers_elbv2.LBAgentHeartbeatOutput, error) {
	var output handlers_elbv2.LBAgentHeartbeatOutput

	if err := ValidateLBAgentHeartbeatInput(input); err != nil {
		return output, err
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.LBAgentHeartbeat(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
