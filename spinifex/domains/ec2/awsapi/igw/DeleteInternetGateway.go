package igw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteInternetGatewayInput(input *ec2.DeleteInternetGatewayInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteInternetGateway handles the EC2 DeleteInternetGateway API call.
func DeleteInternetGateway(ctx context.Context, input *ec2.DeleteInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.DeleteInternetGatewayOutput, error) {
	var output ec2.DeleteInternetGatewayOutput

	if err := ValidateDeleteInternetGatewayInput(input); err != nil {
		return output, err
	}

	svc := ec2igw.NewNATSIGWService(natsConn)
	result, err := svc.DeleteInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
