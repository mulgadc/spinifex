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

// supportedImageAttributes lists the AMI attributes spinifex exposes.
// description is modifiable; blockDeviceMapping is synthesised read-only from
// AMIMetadata. Any other attribute is rejected rather than returning empty.
var supportedImageAttributes = map[string]bool{
	ec2.ImageAttributeNameDescription:        true,
	ec2.ImageAttributeNameBlockDeviceMapping: true,
}

func ValidateDescribeImageAttributeInput(input *ec2.DescribeImageAttributeInput) error {
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
	if !supportedImageAttributes[*input.Attribute] {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func DescribeImageAttribute(ctx context.Context, input *ec2.DescribeImageAttributeInput, natsConn *nats.Conn, accountID string) (ec2.DescribeImageAttributeOutput, error) {
	var output ec2.DescribeImageAttributeOutput

	if err := ValidateDescribeImageAttributeInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_image.NewNATSImageService(natsConn, 0)
	result, err := svc.DescribeImageAttribute(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
