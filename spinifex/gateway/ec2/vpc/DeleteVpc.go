package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteVpcInput(input *ec2.DeleteVpcInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteVpc handles the EC2 DeleteVpc API call
func DeleteVpc(ctx context.Context, input *ec2.DeleteVpcInput, natsConn *nats.Conn, accountID string) (ec2.DeleteVpcOutput, error) {
	var output ec2.DeleteVpcOutput

	if err := ValidateDeleteVpcInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.DeleteVpc(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
