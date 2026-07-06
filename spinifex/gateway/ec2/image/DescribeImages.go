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

func ValidateDescribeImagesInput(input *ec2.DescribeImagesInput) (err error) {
	if input == nil {
		return nil
	}

	if input.ImageIds != nil {
		for _, imageId := range input.ImageIds {
			if imageId != nil && !strings.HasPrefix(*imageId, "ami-") {
				return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
			}
		}
	}

	return err
}

func DescribeImages(ctx context.Context, input *ec2.DescribeImagesInput, natsConn *nats.Conn, accountID string) (output ec2.DescribeImagesOutput, err error) {
	err = ValidateDescribeImagesInput(input)
	if err != nil {
		return output, err
	}

	imageService := handlers_ec2_image.NewNATSImageService(natsConn, 0)
	result, err := imageService.DescribeImages(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
