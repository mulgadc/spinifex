package vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.CreateVpc(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
