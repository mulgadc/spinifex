package instance

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// DefaultCreditSpecification returns AWS's default CPU credit option for a
// burstable instance family, and false for a family that is not burstable.
func DefaultCreditSpecification(family string) (string, bool) {
	switch family {
	case "t2":
		return "standard", true
	case "t3", "t3a", "t4g", "t8i":
		return "unlimited", true
	}
	return "", false
}

// GetDefaultCreditSpecification reports the account's default credit option for
// an instance family. ModifyDefaultCreditSpecification is not supported, so the
// default is always AWS's.
func GetDefaultCreditSpecification(input *ec2.GetDefaultCreditSpecificationInput) (*ec2.GetDefaultCreditSpecificationOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	family := aws.StringValue(input.InstanceFamily)
	if family == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	credits, ok := DefaultCreditSpecification(family)
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInstanceFamily,
			"The %s instance family does not support Unlimited. Only burstable performance instance families support Unlimited. Change the instance family and try again.", family)
	}
	return &ec2.GetDefaultCreditSpecificationOutput{
		InstanceFamilyCreditSpecification: &ec2.InstanceFamilyCreditSpecification{
			InstanceFamily: aws.String(family),
			CpuCredits:     aws.String(credits),
		},
	}, nil
}
