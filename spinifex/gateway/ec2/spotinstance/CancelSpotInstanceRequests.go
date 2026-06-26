package gateway_ec2_spotinstance

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	"github.com/nats-io/nats.go"
)

// CancelSpotInstanceRequests handles the EC2 CancelSpotInstanceRequests API call.
// It is a thin pass-through to the daemon spot service, which moves matching active
// requests to the terminal bucket as cancelled. The instances keep running.
func CancelSpotInstanceRequests(input *ec2.CancelSpotInstanceRequestsInput, natsConn *nats.Conn, accountID string) (ec2.CancelSpotInstanceRequestsOutput, error) {
	var output ec2.CancelSpotInstanceRequestsOutput

	svc := handlers_ec2_spotinstance.NewNATSSpotInstanceService(natsConn)
	result, err := svc.CancelSpotInstanceRequests(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
