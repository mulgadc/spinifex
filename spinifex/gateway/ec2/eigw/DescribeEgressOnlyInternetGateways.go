package gateway_ec2_eigw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eigw "github.com/mulgadc/spinifex/spinifex/domains/ec2/eigw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateDescribeEgressOnlyInternetGatewaysInput(input *ec2.DescribeEgressOnlyInternetGatewaysInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return nil
}

// DescribeEgressOnlyInternetGateways handles the EC2 DescribeEgressOnlyInternetGateways API call.
func DescribeEgressOnlyInternetGateways(ctx context.Context, input *ec2.DescribeEgressOnlyInternetGatewaysInput, natsConn *nats.Conn, accountID string) (ec2.DescribeEgressOnlyInternetGatewaysOutput, error) {
	var output ec2.DescribeEgressOnlyInternetGatewaysOutput

	if err := ValidateDescribeEgressOnlyInternetGatewaysInput(input); err != nil {
		return output, err
	}

	svc := ec2eigw.NewNATSEgressOnlyIGWService(natsConn)
	result, err := svc.DescribeEgressOnlyInternetGateways(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
