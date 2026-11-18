package image

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDisableImageDeregistrationProtectionInput requires a well-formed ImageId.
func ValidateDisableImageDeregistrationProtectionInput(input *ec2.DisableImageDeregistrationProtectionInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return validateProtectionImageID(input.ImageId)
}

// DisableImageDeregistrationProtection implements the EC2 action, forwarding
// it to the NATS image service scoped to accountID.
func DisableImageDeregistrationProtection(ctx context.Context, input *ec2.DisableImageDeregistrationProtectionInput, natsConn *nats.Conn, accountID string) (ec2.DisableImageDeregistrationProtectionOutput, error) {
	var output ec2.DisableImageDeregistrationProtectionOutput
	if err := ValidateDisableImageDeregistrationProtectionInput(input); err != nil {
		return output, err
	}
	result, err := ec2image.NewNATSImageService(natsConn, 0).DisableImageDeregistrationProtection(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
