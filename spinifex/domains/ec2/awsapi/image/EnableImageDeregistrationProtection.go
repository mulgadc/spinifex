package image

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// validateProtectionImageID requires an ImageId of the form DescribeImages accepts.
func validateProtectionImageID(imageID *string) error {
	if imageID == nil || *imageID == "" {
		return missingParam("ImageId")
	}
	suffix, ok := strings.CutPrefix(*imageID, "ami-")
	if !ok || !imageIDSuffixPattern.MatchString(suffix) {
		return awserrors.Errorf(awserrors.ErrorInvalidAMIIDMalformed, "The image ID '%s' is malformed", *imageID)
	}
	return nil
}

// ValidateEnableImageDeregistrationProtectionInput refuses WithCooldown, whose
// 24-hour cooldown is not modelled.
func ValidateEnableImageDeregistrationProtectionInput(input *ec2.EnableImageDeregistrationProtectionInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validateProtectionImageID(input.ImageId); err != nil {
		return err
	}
	if aws.BoolValue(input.WithCooldown) {
		return unsupportedParam("WithCooldown", "true")
	}
	return nil
}

// EnableImageDeregistrationProtection implements the EC2 action, forwarding
// it to the NATS image service scoped to accountID.
func EnableImageDeregistrationProtection(ctx context.Context, input *ec2.EnableImageDeregistrationProtectionInput, natsConn *nats.Conn, accountID string) (ec2.EnableImageDeregistrationProtectionOutput, error) {
	var output ec2.EnableImageDeregistrationProtectionOutput
	if err := ValidateEnableImageDeregistrationProtectionInput(input); err != nil {
		return output, err
	}
	result, err := ec2image.NewNATSImageService(natsConn, 0).EnableImageDeregistrationProtection(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
