package gateway_ec2_eip

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	"github.com/nats-io/nats.go"
)

// ValidateAssociateAddressInput rejects a nil input with InvalidParameterValue and a missing
// AllocationId with MissingParameter; EC2-Classic PublicIp-only requests are not accepted.
func ValidateAssociateAddressInput(input *ec2.AssociateAddressInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.AllocationId == nil || *input.AllocationId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// AssociateAddress handles the EC2 AssociateAddress API call.
func AssociateAddress(ctx context.Context, input *ec2.AssociateAddressInput, natsConn *nats.Conn, accountID string) (ec2.AssociateAddressOutput, error) {
	var output ec2.AssociateAddressOutput

	if err := ValidateAssociateAddressInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_eip.NewNATSEIPService(natsConn)
	result, err := svc.AssociateAddress(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
