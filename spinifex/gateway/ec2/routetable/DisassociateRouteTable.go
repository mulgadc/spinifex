package gateway_ec2_routetable

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	"github.com/nats-io/nats.go"
)

func ValidateDisassociateRouteTableInput(input *ec2.DisassociateRouteTableInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.AssociationId == nil || *input.AssociationId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func DisassociateRouteTable(input *ec2.DisassociateRouteTableInput, natsConn *nats.Conn, accountID string) (ec2.DisassociateRouteTableOutput, error) {
	var output ec2.DisassociateRouteTableOutput
	if err := ValidateDisassociateRouteTableInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.DisassociateRouteTable(input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
