package instance

import (
	"errors"
	"math"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

// The gateway refuses an out-of-range MaxResults first, with AWS's InvalidRequest.
var describeCreditSpecificationsPaging = paging.EC2{
	MaxResults: math.MaxInt64,
	WithIDs:    "The parameter instanceId cannot be used with the parameter maxResults",
}

// DescribeInstanceCreditSpecifications returns CPU credit specifications for T-series instances.
// Stub: always returns "standard" mode for each requested instance.
func DescribeInstanceCreditSpecifications(input *ec2.DescribeInstanceCreditSpecificationsInput) (*ec2.DescribeInstanceCreditSpecificationsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	page, err := describeCreditSpecificationsPaging.Parse(input.MaxResults, input.NextToken, len(input.InstanceIds))
	if err != nil {
		return nil, err
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

	specs, next := paging.EC2Page(specs, func(s *ec2.InstanceCreditSpecification) string { return *s.InstanceId }, page)
	return &ec2.DescribeInstanceCreditSpecificationsOutput{
		InstanceCreditSpecifications: specs,
		NextToken:                    next,
	}, nil
}
