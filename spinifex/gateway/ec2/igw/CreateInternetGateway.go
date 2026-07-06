package gateway_ec2_igw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	"github.com/nats-io/nats.go"
)

// CreateInternetGateway handles the EC2 CreateInternetGateway API call
func CreateInternetGateway(ctx context.Context, input *ec2.CreateInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.CreateInternetGatewayOutput, error) {
	var output ec2.CreateInternetGatewayOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := handlers_ec2_igw.NewNATSIGWService(natsConn)
	result, err := svc.CreateInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
