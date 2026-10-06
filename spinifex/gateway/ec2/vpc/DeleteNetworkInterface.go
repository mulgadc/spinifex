package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteNetworkInterfaceInput(input *ec2.DeleteNetworkInterfaceInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.NetworkInterfaceId == nil || *input.NetworkInterfaceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteNetworkInterface handles the EC2 DeleteNetworkInterface API call.
func DeleteNetworkInterface(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, natsConn *nats.Conn, accountID string) (ec2.DeleteNetworkInterfaceOutput, error) {
	var output ec2.DeleteNetworkInterfaceOutput

	if err := ValidateDeleteNetworkInterfaceInput(input); err != nil {
		return output, err
	}

	svc := ec2vpc.NewNATSVPCService(natsConn)
	result, err := svc.DeleteNetworkInterface(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
