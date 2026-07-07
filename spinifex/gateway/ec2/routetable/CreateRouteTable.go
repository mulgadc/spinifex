package gateway_ec2_routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	"github.com/nats-io/nats.go"
)

func ValidateCreateRouteTableInput(input *ec2.CreateRouteTableInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func CreateRouteTable(ctx context.Context, input *ec2.CreateRouteTableInput, natsConn *nats.Conn, accountID string) (ec2.CreateRouteTableOutput, error) {
	var output ec2.CreateRouteTableOutput
	if err := ValidateCreateRouteTableInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.CreateRouteTable(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
