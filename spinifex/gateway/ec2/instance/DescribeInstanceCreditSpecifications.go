package gateway_ec2_instance

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// DescribeInstanceCreditSpecifications returns CPU credit specifications for T-series instances.
// Stub: always returns "standard" mode for each requested instance.
func DescribeInstanceCreditSpecifications(input *ec2.DescribeInstanceCreditSpecificationsInput) (*ec2.DescribeInstanceCreditSpecificationsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var specs []*ec2.InstanceCreditSpecification
	for _, id := range input.InstanceIds {
		if id != nil && *id != "" {
			specs = append(specs, &ec2.InstanceCreditSpecification{
				InstanceId: id,
				CpuCredits: aws.String("standard"),
			})
		}
	}

	return &ec2.DescribeInstanceCreditSpecificationsOutput{
		InstanceCreditSpecifications: specs,
	}, nil
}
