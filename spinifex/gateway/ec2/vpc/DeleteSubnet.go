package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteSubnetInput(input *ec2.DeleteSubnetInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.SubnetId == nil || *input.SubnetId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteSubnet handles the EC2 DeleteSubnet API call.
func DeleteSubnet(ctx context.Context, input *ec2.DeleteSubnetInput, natsConn *nats.Conn, accountID string) (ec2.DeleteSubnetOutput, error) {
	var output ec2.DeleteSubnetOutput

	if err := ValidateDeleteSubnetInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.DeleteSubnet(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
