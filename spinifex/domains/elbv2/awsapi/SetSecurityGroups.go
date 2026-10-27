package awsapi

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// SetSecurityGroups handles the ELBv2 SetSecurityGroups API call. It replaces
// the security groups associated with the load balancer.
func SetSecurityGroups(ctx context.Context, input *elbv2.SetSecurityGroupsInput, natsConn *nats.Conn, accountID string) (elbv2.SetSecurityGroupsOutput, error) {
	var output elbv2.SetSecurityGroupsOutput

	if input == nil || input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.SecurityGroups) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := elbv2domain.NewNATSELBv2Service(natsConn)
	result, err := svc.SetSecurityGroups(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
