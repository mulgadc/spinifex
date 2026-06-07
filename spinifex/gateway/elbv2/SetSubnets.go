package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// SetSubnets handles the ELBv2 SetSubnets API call. It enables the subnets (and
// their backing ENIs) for the load balancer, adding and removing them to match
// the request.
func SetSubnets(input *elbv2.SetSubnetsInput, natsConn *nats.Conn, accountID string) (elbv2.SetSubnetsOutput, error) {
	var output elbv2.SetSubnetsOutput

	if input == nil || input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Subnets) == 0 && len(input.SubnetMappings) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.SetSubnets(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
