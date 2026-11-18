package vpcpeering

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// AcceptVpcPeeringConnection forwards the action to the peering service scoped to accountID.
func AcceptVpcPeeringConnection(ctx context.Context, input *ec2.AcceptVpcPeeringConnectionInput, natsConn *nats.Conn, accountID string) (ec2.AcceptVpcPeeringConnectionOutput, error) {
	var output ec2.AcceptVpcPeeringConnectionOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := requireConnectionID(input.VpcPeeringConnectionId); err != nil {
		return output, err
	}
	result, err := ec2vpcpeering.NewNATSVpcPeeringService(natsConn).AcceptVpcPeeringConnection(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
