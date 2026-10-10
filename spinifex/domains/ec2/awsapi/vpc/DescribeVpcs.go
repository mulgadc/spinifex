// Package vpc implements the EC2 VPC, subnet, security group and
// network interface actions: it validates each request and forwards it to the
// VPC service over NATS.
package vpc

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// DescribeVpcs handles the EC2 DescribeVpcs API call.
func DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVpcsOutput, error) {
	var output ec2.DescribeVpcsOutput

	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeVpcs(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
