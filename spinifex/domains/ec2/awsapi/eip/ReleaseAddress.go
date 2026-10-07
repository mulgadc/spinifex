package eip

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateReleaseAddressInput(input *ec2.ReleaseAddressInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.AllocationId == nil || *input.AllocationId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ReleaseAddress handles the EC2 ReleaseAddress API call.
func ReleaseAddress(ctx context.Context, input *ec2.ReleaseAddressInput, natsConn *nats.Conn, accountID string) (ec2.ReleaseAddressOutput, error) {
	var output ec2.ReleaseAddressOutput

	if err := ValidateReleaseAddressInput(input); err != nil {
		return output, err
	}

	svc := ec2eip.NewNATSEIPService(natsConn)
	result, err := svc.ReleaseAddress(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
