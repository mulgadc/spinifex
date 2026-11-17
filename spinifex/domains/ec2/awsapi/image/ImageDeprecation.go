package image

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// validateDeprecationImageID requires a well-formed AMI ID, as AWS checks the
// ID's shape before looking the image up.
func validateDeprecationImageID(imageID *string) error {
	if imageID == nil || *imageID == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	suffix, ok := strings.CutPrefix(*imageID, "ami-")
	if !ok || !imageIDSuffixPattern.MatchString(suffix) {
		return awserrors.Errorf(awserrors.ErrorInvalidAMIIDMalformed, "The image ID '%s' is malformed", *imageID)
	}
	return nil
}

// EnableImageDeprecation implements the EC2 EnableImageDeprecation action,
// forwarding to the NATS image service scoped to accountID.
func EnableImageDeprecation(ctx context.Context, input *ec2.EnableImageDeprecationInput, natsConn *nats.Conn, accountID string) (*ec2.EnableImageDeprecationOutput, error) {
	if input == nil || input.DeprecateAt == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validateDeprecationImageID(input.ImageId); err != nil {
		return nil, err
	}
	return ec2image.NewNATSImageService(natsConn, 0).EnableImageDeprecation(ctx, input, accountID)
}

// DisableImageDeprecation implements the EC2 DisableImageDeprecation action,
// forwarding to the NATS image service scoped to accountID.
func DisableImageDeprecation(ctx context.Context, input *ec2.DisableImageDeprecationInput, natsConn *nats.Conn, accountID string) (*ec2.DisableImageDeprecationOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validateDeprecationImageID(input.ImageId); err != nil {
		return nil, err
	}
	return ec2image.NewNATSImageService(natsConn, 0).DisableImageDeprecation(ctx, input, accountID)
}
