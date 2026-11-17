package vpcpeering

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DeleteVpcPeeringConnection forwards the action to the peering service scoped to accountID.
func DeleteVpcPeeringConnection(ctx context.Context, input *ec2.DeleteVpcPeeringConnectionInput, natsConn *nats.Conn, accountID string) (ec2.DeleteVpcPeeringConnectionOutput, error) {
	var output ec2.DeleteVpcPeeringConnectionOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := requireConnectionID(input.VpcPeeringConnectionId); err != nil {
		return output, err
	}
	result, err := ec2vpcpeering.NewNATSVpcPeeringService(natsConn).DeleteVpcPeeringConnection(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
