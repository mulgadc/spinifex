package gateway_ec2_igw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	"github.com/nats-io/nats.go"
)

// DescribeInternetGateways handles the EC2 DescribeInternetGateways API call.
func DescribeInternetGateways(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, natsConn *nats.Conn, accountID string) (ec2.DescribeInternetGatewaysOutput, error) {
	var output ec2.DescribeInternetGatewaysOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := handlers_ec2_igw.NewNATSIGWService(natsConn)
	result, err := svc.DescribeInternetGateways(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
