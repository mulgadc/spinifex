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

// ValidateResetImageAttributeInput accepts only description; launchPermission
// is out of scope.
func ValidateResetImageAttributeInput(input *ec2.ResetImageAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.ImageId == nil || *input.ImageId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !strings.HasPrefix(*input.ImageId, "ami-") {
		return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}
	if input.Attribute == nil || *input.Attribute == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if *input.Attribute != ec2.ImageAttributeNameDescription {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func ResetImageAttribute(ctx context.Context, input *ec2.ResetImageAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ResetImageAttributeOutput, error) {
	var output ec2.ResetImageAttributeOutput

	if err := ValidateResetImageAttributeInput(input); err != nil {
		return output, err
	}

	svc := ec2image.NewNATSImageService(natsConn, 0)
	result, err := svc.ResetImageAttribute(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
