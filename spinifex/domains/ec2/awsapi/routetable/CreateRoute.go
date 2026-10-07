package routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

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

func CreateRoute(ctx context.Context, input *ec2.CreateRouteInput, natsConn *nats.Conn, accountID string) (ec2.CreateRouteOutput, error) {
	var output ec2.CreateRouteOutput
	if err := ValidateCreateRouteInput(input); err != nil {
		return output, err
	}
	svc := ec2routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.CreateRoute(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
