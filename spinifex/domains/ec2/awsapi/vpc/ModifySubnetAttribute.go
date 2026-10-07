package vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

// ModifySubnetAttribute handles the EC2 ModifySubnetAttribute API call.
func ModifySubnetAttribute(ctx context.Context, input *ec2.ModifySubnetAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ModifySubnetAttributeOutput, error) {
	var output ec2.ModifySubnetAttributeOutput

	if err := ValidateModifySubnetAttributeInput(input); err != nil {
		return output, err
	}

	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.ModifySubnetAttribute(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
