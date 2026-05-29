package gateway_ec2_vpc

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateCreateSubnetInput(input *ec2.CreateSubnetInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.CidrBlock == nil || *input.CidrBlock == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateSubnet handles the EC2 CreateSubnet API call
func CreateSubnet(input *ec2.CreateSubnetInput, natsConn *nats.Conn, accountID string) (ec2.CreateSubnetOutput, error) {
	var output ec2.CreateSubnetOutput

	if err := ValidateCreateSubnetInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.CreateSubnet(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
