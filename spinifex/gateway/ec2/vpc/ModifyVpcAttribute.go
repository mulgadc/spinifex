package gateway_ec2_vpc

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateModifyVpcAttributeInput(input *ec2.ModifyVpcAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ModifyVpcAttribute handles the EC2 ModifyVpcAttribute API call
func ModifyVpcAttribute(input *ec2.ModifyVpcAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ModifyVpcAttributeOutput, error) {
	var output ec2.ModifyVpcAttributeOutput

	if err := ValidateModifyVpcAttributeInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.ModifyVpcAttribute(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
