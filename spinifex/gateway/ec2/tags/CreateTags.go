package gateway_ec2_tags

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	"github.com/nats-io/nats.go"
)

// ValidateCreateTagsInput validates the input parameters for CreateTags
func ValidateCreateTagsInput(input *ec2.CreateTagsInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if len(input.Resources) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if len(input.Tags) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	for _, tag := range input.Tags {
		if tag.Key == nil || *tag.Key == "" {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	return nil
}

// CreateTags handles the EC2 CreateTags API call
func CreateTags(ctx context.Context, input *ec2.CreateTagsInput, natsConn *nats.Conn, accountID string) (ec2.CreateTagsOutput, error) {
	var output ec2.CreateTagsOutput

	if err := ValidateCreateTagsInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_tags.NewNATSTagsService(natsConn)
	result, err := svc.CreateTags(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
