package gateway_ec2_routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteRouteTableInput(input *ec2.DeleteRouteTableInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func DeleteRouteTable(ctx context.Context, input *ec2.DeleteRouteTableInput, natsConn *nats.Conn, accountID string) (ec2.DeleteRouteTableOutput, error) {
	var output ec2.DeleteRouteTableOutput
	if err := ValidateDeleteRouteTableInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.DeleteRouteTable(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
