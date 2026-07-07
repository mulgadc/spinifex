package gateway_ec2_eip

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	"github.com/nats-io/nats.go"
)

// DescribeAddresses handles the EC2 DescribeAddresses API call.
func DescribeAddresses(ctx context.Context, input *ec2.DescribeAddressesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeAddressesOutput, error) {
	var output ec2.DescribeAddressesOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := handlers_ec2_eip.NewNATSEIPService(natsConn)
	result, err := svc.DescribeAddresses(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
