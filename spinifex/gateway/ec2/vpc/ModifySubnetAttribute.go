package gateway_ec2_vpc

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateModifySubnetAttributeInput(input *ec2.ModifySubnetAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.SubnetId == nil || *input.SubnetId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ModifySubnetAttribute handles the EC2 ModifySubnetAttribute API call
func ModifySubnetAttribute(input *ec2.ModifySubnetAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ModifySubnetAttributeOutput, error) {
	var output ec2.ModifySubnetAttributeOutput

	if err := ValidateModifySubnetAttributeInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.ModifySubnetAttribute(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
