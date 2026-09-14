package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// MaxResults bounds from the EC2 model. A raw query request bypasses the
// client-side validation an SDK caller gets, so the range is enforced here
// rather than silently clamped into a page size the caller did not ask for.
const (
	minSGForVpcResults = 5
	maxSGForVpcResults = 1000
)

// VpcId is required by the model, so an absent one is MissingParameter before
// the request crosses NATS. Whether it names a VPC is the handler's call: only
// the store knows.
func GetSecurityGroupsForVpc(ctx context.Context, input *ec2.GetSecurityGroupsForVpcInput, natsConn *nats.Conn, accountID string) (ec2.GetSecurityGroupsForVpcOutput, error) {
	var output ec2.GetSecurityGroupsForVpcOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.MaxResults != nil && (*input.MaxResults < minSGForVpcResults || *input.MaxResults > maxSGForVpcResults) {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.GetSecurityGroupsForVpc(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
