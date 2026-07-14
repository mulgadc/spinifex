package gateway_ec2_image

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	"github.com/nats-io/nats.go"
)

// ValidateModifyImageAttributeInput validates and normalises the two AWS
// shapes for modifying description (top-level `--description Value=…` and
// structured `--attribute description --value …`) into Attribute+Value.
// Unsupported top-level shortcuts (launchPermission, imdsSupport, productCodes)
// are rejected rather than silently accepted.
func ValidateModifyImageAttributeInput(input *ec2.ModifyImageAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.ImageId == nil || *input.ImageId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !strings.HasPrefix(*input.ImageId, "ami-") {
		return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	if input.LaunchPermission != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ImdsSupport != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.ProductCodes) > 0 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.UserIds) > 0 || len(input.UserGroups) > 0 ||
		len(input.OrganizationArns) > 0 || len(input.OrganizationalUnitArns) > 0 ||
		(input.OperationType != nil && *input.OperationType != "") {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	hasTopLevelDescription := input.Description != nil
	// Treat any set Value (even without Attribute) as structured shape so it
	// isn't silently discarded by the top-level branch.
	hasStructured := (input.Attribute != nil && *input.Attribute != "") || input.Value != nil

	switch {
	case hasTopLevelDescription && hasStructured:
		return errors.New(awserrors.ErrorInvalidParameterCombination)
	case hasTopLevelDescription:
		value := ""
		if input.Description.Value != nil {
			value = *input.Description.Value
		}
		input.Attribute = aws.String(ec2.ImageAttributeNameDescription)
		input.Value = aws.String(value)
	case hasStructured:
		if input.Attribute == nil || *input.Attribute == "" {
			return errors.New(awserrors.ErrorMissingParameter)
		}
		if *input.Attribute != ec2.ImageAttributeNameDescription {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	default:
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return nil
}

func ModifyImageAttribute(ctx context.Context, input *ec2.ModifyImageAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ModifyImageAttributeOutput, error) {
	var output ec2.ModifyImageAttributeOutput

	if err := ValidateModifyImageAttributeInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_image.NewNATSImageService(natsConn, 0)
	result, err := svc.ModifyImageAttribute(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
