package routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteRouteInput rejects a nil input with InvalidParameterValue and a missing
// RouteTableId or DestinationCidrBlock with MissingParameter.
func ValidateDeleteRouteInput(input *ec2.DeleteRouteInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.DestinationCidrBlock == nil || *input.DestinationCidrBlock == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteRoute implements the EC2 DeleteRoute action, removing the route for DestinationCidrBlock
// from a route table via the NATS route table service.
func DeleteRoute(ctx context.Context, input *ec2.DeleteRouteInput, natsConn *nats.Conn, accountID string) (ec2.DeleteRouteOutput, error) {
	var output ec2.DeleteRouteOutput
	if err := ValidateDeleteRouteInput(input); err != nil {
		return output, err
	}
	svc := ec2routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.DeleteRoute(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
