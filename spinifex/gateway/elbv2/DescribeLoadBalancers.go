package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// DescribeLoadBalancers handles the ELBv2 DescribeLoadBalancers API call.
func DescribeLoadBalancers(ctx context.Context, input *elbv2.DescribeLoadBalancersInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeLoadBalancersOutput, error) {
	var output elbv2.DescribeLoadBalancersOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeLoadBalancers(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
