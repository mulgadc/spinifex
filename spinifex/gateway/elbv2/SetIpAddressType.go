package gateway_elbv2

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// SetIpAddressType handles the ELBv2 SetIpAddressType API call. It sets the IP
// address type of the load balancer.
func SetIpAddressType(ctx context.Context, input *elbv2.SetIpAddressTypeInput, natsConn *nats.Conn, accountID string) (elbv2.SetIpAddressTypeOutput, error) {
	var output elbv2.SetIpAddressTypeOutput

	if input == nil || input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.IpAddressType == nil || *input.IpAddressType == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.SetIpAddressType(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
