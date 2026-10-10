// Package igw implements the EC2 internet gateway actions: it
// validates each request and forwards it to the IGW service over NATS.
package igw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeInternetGateways handles the EC2 DescribeInternetGateways API call.
func DescribeInternetGateways(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, natsConn *nats.Conn, accountID string) (ec2.DescribeInternetGatewaysOutput, error) {
	var output ec2.DescribeInternetGatewaysOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := ec2igw.NewNATSIGWService(natsConn)
	result, err := svc.DescribeInternetGateways(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
