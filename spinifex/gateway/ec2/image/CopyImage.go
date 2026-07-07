package gateway_ec2_image

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	"github.com/nats-io/nats.go"
)

// ValidateCopyImageInput validates the input parameters for CopyImage.
// Copy is single-region and metadata-only; cross-region, encryption, and
// Outposts are rejected here. ClientToken is accepted but not honoured.
func ValidateCopyImageInput(input *ec2.CopyImageInput, gwRegion string) error {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.Name == nil || *input.Name == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if n := len(*input.Name); n < 3 || n > 128 {
		return errors.New(awserrors.ErrorInvalidAMINameMalformed)
	}

	if input.SourceImageId == nil || *input.SourceImageId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !strings.HasPrefix(*input.SourceImageId, "ami-") {
		return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	if input.SourceRegion == nil || *input.SourceRegion == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if *input.SourceRegion != gwRegion {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.Encrypted != nil && *input.Encrypted {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.KmsKeyId != nil && *input.KmsKeyId != "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.DestinationOutpostArn != nil && *input.DestinationOutpostArn != "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return nil
}

// CopyImage handles the EC2 CopyImage API call.
func CopyImage(ctx context.Context, input *ec2.CopyImageInput, natsConn *nats.Conn, gwRegion, accountID string) (ec2.CopyImageOutput, error) {
	var output ec2.CopyImageOutput

	if err := ValidateCopyImageInput(input, gwRegion); err != nil {
		return output, err
	}

	svc := handlers_ec2_image.NewNATSImageService(natsConn, 0)
	result, err := svc.CopyImage(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
