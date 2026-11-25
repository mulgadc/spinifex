package tgw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeTransitGateways returns accountID's Transit Gateways.
func DescribeTransitGateways(ctx context.Context, input *ec2.DescribeTransitGatewaysInput, natsConn *nats.Conn, accountID string) (ec2.DescribeTransitGatewaysOutput, error) {
	var output ec2.DescribeTransitGatewaysOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	result, err := ec2tgw.NewNATSTransitGatewayService(natsConn).DescribeTransitGateways(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
