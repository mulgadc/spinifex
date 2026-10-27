package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeListeners handles the ELBv2 DescribeListeners API call.
func DescribeListeners(ctx context.Context, input *elbv2.DescribeListenersInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeListenersOutput, error) {
	var output elbv2.DescribeListenersOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeListeners(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
