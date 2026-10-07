package vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateModifyNetworkInterfaceAttributeInput(input *ec2.ModifyNetworkInterfaceAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.NetworkInterfaceId == nil || *input.NetworkInterfaceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return ec2vpc.ValidateModifyNetworkInterfaceAttributeAttributes(input)
}

// ModifyNetworkInterfaceAttribute handles the EC2 ModifyNetworkInterfaceAttribute API call.
func ModifyNetworkInterfaceAttribute(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	var output ec2.ModifyNetworkInterfaceAttributeOutput

	if err := ValidateModifyNetworkInterfaceAttributeInput(input); err != nil {
		return output, err
	}

	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.ModifyNetworkInterfaceAttribute(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
