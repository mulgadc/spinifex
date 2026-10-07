package eigw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eigw "github.com/mulgadc/spinifex/spinifex/domains/ec2/eigw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateCreateEgressOnlyInternetGatewayInput rejects a nil input with InvalidParameterValue and
// a missing VpcId with MissingParameter.
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

	svc := ec2eigw.NewNATSEgressOnlyIGWService(natsConn)
	result, err := svc.CreateEgressOnlyInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
