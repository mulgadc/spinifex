package eip

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeAddressesAttribute handles the EC2 DescribeAddressesAttribute API call.
func DescribeAddressesAttribute(ctx context.Context, input *ec2.DescribeAddressesAttributeInput, natsConn *nats.Conn, accountID string) (ec2.DescribeAddressesAttributeOutput, error) {
	var output ec2.DescribeAddressesAttributeOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := ec2eip.NewNATSEIPService(natsConn)
	result, err := svc.DescribeAddressesAttribute(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
