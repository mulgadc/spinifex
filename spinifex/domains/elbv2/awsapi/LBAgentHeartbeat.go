package awsapi

import (
	"context"
	"errors"

	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateLBAgentHeartbeatInput rejects a nil input with InvalidParameterValue and a missing LBID
// with MissingParameter.
func ValidateLBAgentHeartbeatInput(input *elbv2domain.LBAgentHeartbeatInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.LBID == nil || *input.LBID == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// LBAgentHeartbeat handles the ELBv2 LBAgentHeartbeat API call.
func LBAgentHeartbeat(ctx context.Context, input *elbv2domain.LBAgentHeartbeatInput, natsConn *nats.Conn, accountID string) (elbv2domain.LBAgentHeartbeatOutput, error) {
	var output elbv2domain.LBAgentHeartbeatOutput

	if err := ValidateLBAgentHeartbeatInput(input); err != nil {
		return output, err
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.LBAgentHeartbeat(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
