package gateway_ec2_spotinstance

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	"github.com/nats-io/nats.go"
)

// DescribeSpotInstanceRequests handles the EC2 DescribeSpotInstanceRequests API
// call. It is a thin pass-through to the daemon spot service, which reads both the
// active and terminal buckets, merges, and applies the requested filters.
func DescribeSpotInstanceRequests(input *ec2.DescribeSpotInstanceRequestsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSpotInstanceRequestsOutput, error) {
	var output ec2.DescribeSpotInstanceRequestsOutput

	svc := handlers_ec2_spotinstance.NewNATSSpotInstanceService(natsConn)
	result, err := svc.DescribeSpotInstanceRequests(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
