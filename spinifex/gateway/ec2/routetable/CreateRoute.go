package gateway_ec2_routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	"github.com/nats-io/nats.go"
)

// ValidateCreateRouteInput rejects a nil input with InvalidParameterValue and a missing
// RouteTableId or DestinationCidrBlock with MissingParameter.
func ValidateCreateRouteInput(input *ec2.CreateRouteInput) error {
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

// CreateRoute implements the EC2 CreateRoute action, adding an IPv4 route to a route table via
// the NATS route table service.
func CreateRoute(ctx context.Context, input *ec2.CreateRouteInput, natsConn *nats.Conn, accountID string) (ec2.CreateRouteOutput, error) {
	var output ec2.CreateRouteOutput
	if err := ValidateCreateRouteInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.CreateRoute(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
