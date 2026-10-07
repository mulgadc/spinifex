// Package eip implements the EC2 Elastic IP actions: it validates
// each request and forwards it to the EIP service over NATS.
package eip

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeAddresses handles the EC2 DescribeAddresses API call.
func DescribeAddresses(ctx context.Context, input *ec2.DescribeAddressesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeAddressesOutput, error) {
	var output ec2.DescribeAddressesOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := ec2eip.NewNATSEIPService(natsConn)
	result, err := svc.DescribeAddresses(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
