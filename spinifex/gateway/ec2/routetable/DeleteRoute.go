package gateway_ec2_routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	"github.com/nats-io/nats.go"
)

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

func DeleteRoute(ctx context.Context, input *ec2.DeleteRouteInput, natsConn *nats.Conn, accountID string) (ec2.DeleteRouteOutput, error) {
	var output ec2.DeleteRouteOutput
	if err := ValidateDeleteRouteInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.DeleteRoute(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
