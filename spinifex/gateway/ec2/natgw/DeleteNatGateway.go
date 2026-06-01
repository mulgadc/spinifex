package gateway_ec2_natgw

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteNatGatewayInput(input *ec2.DeleteNatGatewayInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.NatGatewayId == nil || *input.NatGatewayId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func DeleteNatGateway(input *ec2.DeleteNatGatewayInput, natsConn *nats.Conn, accountID string) (ec2.DeleteNatGatewayOutput, error) {
	var output ec2.DeleteNatGatewayOutput
	if err := ValidateDeleteNatGatewayInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_natgw.NewNATSNatGatewayService(natsConn)
	result, err := svc.DeleteNatGateway(input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
