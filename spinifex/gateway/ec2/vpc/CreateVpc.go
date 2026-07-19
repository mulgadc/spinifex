package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateCreateVpcInput(input *ec2.CreateVpcInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.CidrBlock == nil || *input.CidrBlock == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateVpc handles the EC2 CreateVpc API call.
func CreateVpc(ctx context.Context, input *ec2.CreateVpcInput, natsConn *nats.Conn, accountID string) (ec2.CreateVpcOutput, error) {
	var output ec2.CreateVpcOutput

	if err := ValidateCreateVpcInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.CreateVpc(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
