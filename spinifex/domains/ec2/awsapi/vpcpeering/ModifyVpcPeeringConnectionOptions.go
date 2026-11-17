package vpcpeering

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ModifyVpcPeeringConnectionOptions forwards an options change to the peering service scoped to accountID.
func ModifyVpcPeeringConnectionOptions(ctx context.Context, input *ec2.ModifyVpcPeeringConnectionOptionsInput, natsConn *nats.Conn, accountID string) (ec2.ModifyVpcPeeringConnectionOptionsOutput, error) {
	var output ec2.ModifyVpcPeeringConnectionOptionsOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := requireConnectionID(input.VpcPeeringConnectionId); err != nil {
		return output, err
	}
	result, err := ec2vpcpeering.NewNATSVpcPeeringService(natsConn).ModifyVpcPeeringConnectionOptions(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
