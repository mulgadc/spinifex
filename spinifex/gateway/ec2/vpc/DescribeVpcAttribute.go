package gateway_ec2_vpc

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateDescribeVpcAttributeInput(input *ec2.DescribeVpcAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Attribute == nil || *input.Attribute == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DescribeVpcAttribute handles the EC2 DescribeVpcAttribute API call
func DescribeVpcAttribute(input *ec2.DescribeVpcAttributeInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVpcAttributeOutput, error) {
	var output ec2.DescribeVpcAttributeOutput

	if err := ValidateDescribeVpcAttributeInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.DescribeVpcAttribute(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
