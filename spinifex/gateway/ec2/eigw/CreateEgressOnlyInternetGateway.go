package gateway_ec2_eigw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eigw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	"github.com/nats-io/nats.go"
)

func ValidateCreateEgressOnlyInternetGatewayInput(input *ec2.CreateEgressOnlyInternetGatewayInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return nil
}

// CreateEgressOnlyInternetGateway handles the EC2 CreateEgressOnlyInternetGateway API call.
func CreateEgressOnlyInternetGateway(ctx context.Context, input *ec2.CreateEgressOnlyInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.CreateEgressOnlyInternetGatewayOutput, error) {
	var output ec2.CreateEgressOnlyInternetGatewayOutput

	if err := ValidateCreateEgressOnlyInternetGatewayInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_eigw.NewNATSEgressOnlyIGWService(natsConn)
	result, err := svc.CreateEgressOnlyInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
