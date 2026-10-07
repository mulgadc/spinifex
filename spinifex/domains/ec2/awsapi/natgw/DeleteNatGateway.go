package natgw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2natgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/natgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

func DeleteNatGateway(ctx context.Context, input *ec2.DeleteNatGatewayInput, natsConn *nats.Conn, accountID string) (ec2.DeleteNatGatewayOutput, error) {
	var output ec2.DeleteNatGatewayOutput
	if err := ValidateDeleteNatGatewayInput(input); err != nil {
		return output, err
	}
	svc := ec2natgw.NewNATSNatGatewayService(natsConn)
	result, err := svc.DeleteNatGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
