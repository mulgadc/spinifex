package gateway_ec2_routetable

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	"github.com/nats-io/nats.go"
)

func ValidateAssociateRouteTableInput(input *ec2.AssociateRouteTableInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.SubnetId == nil || *input.SubnetId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func AssociateRouteTable(input *ec2.AssociateRouteTableInput, natsConn *nats.Conn, accountID string) (ec2.AssociateRouteTableOutput, error) {
	var output ec2.AssociateRouteTableOutput
	if err := ValidateAssociateRouteTableInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.AssociateRouteTable(input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
