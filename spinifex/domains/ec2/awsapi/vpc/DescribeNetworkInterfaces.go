package vpc

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// DescribeNetworkInterfaces handles the EC2 DescribeNetworkInterfaces API call.
func DescribeNetworkInterfaces(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeNetworkInterfacesOutput, error) {
	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeNetworkInterfaces(ctx, input, accountID)
	if err != nil {
		return ec2.DescribeNetworkInterfacesOutput{}, err
	}

	return *result, nil
}
