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

func ValidateDeregisterImageInput(input *ec2.DeregisterImageInput) error {
	if input == nil || input.ImageId == nil || *input.ImageId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !strings.HasPrefix(*input.ImageId, "ami-") {
		return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}
	return nil
}

func DeregisterImage(ctx context.Context, input *ec2.DeregisterImageInput, natsConn *nats.Conn, accountID string) (ec2.DeregisterImageOutput, error) {
	var output ec2.DeregisterImageOutput

	if err := ValidateDeregisterImageInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_image.NewNATSImageService(natsConn, 0)
	result, err := svc.DeregisterImage(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
