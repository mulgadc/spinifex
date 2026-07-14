package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateModifyNetworkInterfaceAttributeInput(input *ec2.ModifyNetworkInterfaceAttributeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.NetworkInterfaceId == nil || *input.NetworkInterfaceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Groups) == 0 && input.Description == nil && input.SourceDestCheck == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	// Disabling source/dest check is unsupported: OVN port security enforces it.
	if input.SourceDestCheck != nil && input.SourceDestCheck.Value != nil && !*input.SourceDestCheck.Value {
		return errors.New(awserrors.ErrorUnsupported)
	}
	return nil
}

// ModifyNetworkInterfaceAttribute handles the EC2 ModifyNetworkInterfaceAttribute API call
func ModifyNetworkInterfaceAttribute(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	var output ec2.ModifyNetworkInterfaceAttributeOutput

	if err := ValidateModifyNetworkInterfaceAttributeInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.ModifyNetworkInterfaceAttribute(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
