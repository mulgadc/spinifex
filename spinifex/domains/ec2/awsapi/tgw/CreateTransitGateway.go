// Package tgw implements the EC2 Transit Gateway actions: it forwards each
// request over NATS to the Transit Gateway service.
package tgw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// CreateTransitGateway forwards the action to the Transit Gateway service scoped to accountID.
func CreateTransitGateway(ctx context.Context, input *ec2.CreateTransitGatewayInput, natsConn *nats.Conn, accountID string) (ec2.CreateTransitGatewayOutput, error) {
	var output ec2.CreateTransitGatewayOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	result, err := ec2tgw.NewNATSTransitGatewayService(natsConn).CreateTransitGateway(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
