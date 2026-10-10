// Package eigw implements the EC2 egress-only internet gateway
// actions: it validates each request and forwards it to the EIGW service over
// NATS.
package eigw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eigw "github.com/mulgadc/spinifex/spinifex/domains/ec2/eigw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDescribeEgressOnlyInternetGatewaysInput rejects only a nil input, with
// InvalidParameterValue; IDs and filters are checked by the handler.
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
