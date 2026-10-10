package eip

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// AllocateAddress handles the EC2 AllocateAddress API call.
func AllocateAddress(ctx context.Context, input *ec2.AllocateAddressInput, natsConn *nats.Conn, accountID string) (ec2.AllocateAddressOutput, error) {
	var output ec2.AllocateAddressOutput

	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	svc := ec2eip.NewNATSEIPService(natsConn)
	result, err := svc.AllocateAddress(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
