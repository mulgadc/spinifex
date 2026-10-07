package tags

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteTagsInput validates the input parameters for DeleteTags.
func ValidateDeleteTagsInput(input *ec2.DeleteTagsInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if len(input.Resources) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return nil
}

// DeleteTags handles the EC2 DeleteTags API call.
func DeleteTags(ctx context.Context, input *ec2.DeleteTagsInput, natsConn *nats.Conn, accountID string) (ec2.DeleteTagsOutput, error) {
	var output ec2.DeleteTagsOutput

	if err := ValidateDeleteTagsInput(input); err != nil {
		return output, err
	}

	svc := ec2tags.NewNATSTagsService(natsConn)
	result, err := svc.DeleteTags(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
