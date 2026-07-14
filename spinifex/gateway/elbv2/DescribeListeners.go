package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// DescribeListeners handles the ELBv2 DescribeListeners API call.
func DescribeListeners(ctx context.Context, input *elbv2.DescribeListenersInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeListenersOutput, error) {
	var output elbv2.DescribeListenersOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeListeners(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
