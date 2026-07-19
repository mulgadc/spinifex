package gateway_ec2_igw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	"github.com/nats-io/nats.go"
)

func ValidateAttachInternetGatewayInput(input *ec2.AttachInternetGatewayInput) error {
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

// AttachInternetGateway handles the EC2 AttachInternetGateway API call.
func AttachInternetGateway(ctx context.Context, input *ec2.AttachInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.AttachInternetGatewayOutput, error) {
	var output ec2.AttachInternetGatewayOutput

	if err := ValidateAttachInternetGatewayInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_igw.NewNATSIGWService(natsConn)
	result, err := svc.AttachInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
