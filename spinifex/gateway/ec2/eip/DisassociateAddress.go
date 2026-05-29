package gateway_ec2_eip

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	"github.com/nats-io/nats.go"
)

func ValidateDisassociateAddressInput(input *ec2.DisassociateAddressInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.AssociationId == nil || *input.AssociationId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DisassociateAddress handles the EC2 DisassociateAddress API call.
func DisassociateAddress(input *ec2.DisassociateAddressInput, natsConn *nats.Conn, accountID string) (ec2.DisassociateAddressOutput, error) {
	var output ec2.DisassociateAddressOutput

	if err := ValidateDisassociateAddressInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_eip.NewNATSEIPService(natsConn)
	result, err := svc.DisassociateAddress(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
