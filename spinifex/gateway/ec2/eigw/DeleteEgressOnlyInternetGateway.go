package gateway_ec2_eigw

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eigw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteEgressOnlyInternetGatewayInput(input *ec2.DeleteEgressOnlyInternetGatewayInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.EgressOnlyInternetGatewayId == nil || *input.EgressOnlyInternetGatewayId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return nil
}

// DeleteEgressOnlyInternetGateway handles the EC2 DeleteEgressOnlyInternetGateway API call
func DeleteEgressOnlyInternetGateway(input *ec2.DeleteEgressOnlyInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.DeleteEgressOnlyInternetGatewayOutput, error) {
	var output ec2.DeleteEgressOnlyInternetGatewayOutput

	if err := ValidateDeleteEgressOnlyInternetGatewayInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_eigw.NewNATSEgressOnlyIGWService(natsConn)
	result, err := svc.DeleteEgressOnlyInternetGateway(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
