// Package natgw implements the EC2 NAT gateway actions: it
// validates each request and forwards it to the NAT gateway service over NATS.
package natgw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2natgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/natgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeNatGateways implements the EC2 DescribeNatGateways action, returning accountID's NAT
// gateways from the NATS service. A nil input is rejected with InvalidParameterValue.
func DescribeNatGateways(ctx context.Context, input *ec2.DescribeNatGatewaysInput, natsConn *nats.Conn, accountID string) (ec2.DescribeNatGatewaysOutput, error) {
	var output ec2.DescribeNatGatewaysOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	svc := ec2natgw.NewNATSNatGatewayService(natsConn)
	result, err := svc.DescribeNatGateways(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
