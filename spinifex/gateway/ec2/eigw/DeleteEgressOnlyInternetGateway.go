package gateway_ec2_eigw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eigw "github.com/mulgadc/spinifex/spinifex/domains/ec2/eigw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

// DeleteEgressOnlyInternetGateway handles the EC2 DeleteEgressOnlyInternetGateway API call.
func DeleteEgressOnlyInternetGateway(ctx context.Context, input *ec2.DeleteEgressOnlyInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.DeleteEgressOnlyInternetGatewayOutput, error) {
	var output ec2.DeleteEgressOnlyInternetGatewayOutput

	if err := ValidateDeleteEgressOnlyInternetGatewayInput(input); err != nil {
		return output, err
	}

	svc := ec2eigw.NewNATSEgressOnlyIGWService(natsConn)
	result, err := svc.DeleteEgressOnlyInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
