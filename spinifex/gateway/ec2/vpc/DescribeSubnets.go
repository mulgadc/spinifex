package gateway_ec2_vpc

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// DescribeSubnets handles the EC2 DescribeSubnets API call.
func DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSubnetsOutput, error) {
	var output ec2.DescribeSubnetsOutput

	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeSubnets(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
