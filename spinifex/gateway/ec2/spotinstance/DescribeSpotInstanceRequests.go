package gateway_ec2_spotinstance

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	"github.com/nats-io/nats.go"
)

// DescribeSpotInstanceRequests handles the EC2 DescribeSpotInstanceRequests API
// call. It is a thin pass-through to the daemon spot service, which reads both the
// active and terminal buckets, merges, and applies the requested filters.
func DescribeSpotInstanceRequests(ctx context.Context, input *ec2.DescribeSpotInstanceRequestsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSpotInstanceRequestsOutput, error) {
	var output ec2.DescribeSpotInstanceRequestsOutput

	svc := ec2spotinstance.NewNATSSpotInstanceService(natsConn)
	result, err := svc.DescribeSpotInstanceRequests(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
