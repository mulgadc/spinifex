package gateway_ec2_vpc

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// DescribeVpcs handles the EC2 DescribeVpcs API call
func DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVpcsOutput, error) {
	var output ec2.DescribeVpcsOutput

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeVpcs(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
