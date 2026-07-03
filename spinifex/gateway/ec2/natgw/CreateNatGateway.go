package gateway_ec2_natgw

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	"github.com/nats-io/nats.go"
)

func ValidateCreateNatGatewayInput(input *ec2.CreateNatGatewayInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.SubnetId == nil || *input.SubnetId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	// Private NAT gateways (ConnectivityType=private) are not implemented.
	if input.ConnectivityType != nil && *input.ConnectivityType == "private" {
		return errors.New(awserrors.ErrorUnsupported)
	}
	// AllocationId is required for public NAT gateways, matching AWS.
	if input.AllocationId == nil || *input.AllocationId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

func CreateNatGateway(input *ec2.CreateNatGatewayInput, natsConn *nats.Conn, accountID string) (ec2.CreateNatGatewayOutput, error) {
	var output ec2.CreateNatGatewayOutput
	if err := ValidateCreateNatGatewayInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_natgw.NewNATSNatGatewayService(natsConn)
	result, err := svc.CreateNatGateway(input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
