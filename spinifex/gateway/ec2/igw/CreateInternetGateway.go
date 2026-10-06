package gateway_ec2_igw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// CreateInternetGateway handles the EC2 CreateInternetGateway API call.
func CreateInternetGateway(ctx context.Context, input *ec2.CreateInternetGatewayInput, natsConn *nats.Conn, accountID string) (ec2.CreateInternetGatewayOutput, error) {
	var output ec2.CreateInternetGatewayOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := ec2igw.NewNATSIGWService(natsConn)
	result, err := svc.CreateInternetGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
