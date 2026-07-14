package gateway_ec2_vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
)

func ValidateCreateNetworkInterfaceInput(input *ec2.CreateNetworkInterfaceInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.SubnetId == nil || *input.SubnetId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateNetworkInterface handles the EC2 CreateNetworkInterface API call
func CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, natsConn *nats.Conn, accountID string) (ec2.CreateNetworkInterfaceOutput, error) {
	var output ec2.CreateNetworkInterfaceOutput

	if err := ValidateCreateNetworkInterfaceInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_vpc.NewNATSVPCService(natsConn)
	result, err := svc.CreateNetworkInterface(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
