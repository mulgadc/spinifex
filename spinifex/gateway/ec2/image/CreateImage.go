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

// ValidateCreateImageInput validates the input parameters for CreateImage.
func ValidateCreateImageInput(input *ec2.CreateImageInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.InstanceId == nil || *input.InstanceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if !strings.HasPrefix(*input.InstanceId, "i-") {
		return errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}

	if input.Name == nil || *input.Name == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return nil
}

// CreateImage handles the EC2 CreateImage API call.
func CreateImage(ctx context.Context, input *ec2.CreateImageInput, natsConn *nats.Conn, expectedNodes int, accountID string) (ec2.CreateImageOutput, error) {
	var output ec2.CreateImageOutput

	if err := ValidateCreateImageInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_image.NewNATSImageService(natsConn, expectedNodes)
	result, err := svc.CreateImage(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	if result == nil {
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	return *result, nil
}
