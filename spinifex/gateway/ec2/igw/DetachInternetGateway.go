package gateway_ec2_igw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	"github.com/nats-io/nats.go"
)

func ValidateDetachInternetGatewayInput(input *ec2.DetachInternetGatewayInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DetachInternetGateway handles the EC2 DetachInternetGateway API call.
func DetachInternetGateway(ctx context.Context, input *ec2.DetachInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.DetachInternetGatewayOutput, error) {
	var output ec2.DetachInternetGatewayOutput

	if err := ValidateDetachInternetGatewayInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_igw.NewNATSIGWService(natsConn)
	result, err := svc.DetachInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
