package routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateCreateRouteTableInput rejects a nil input with InvalidParameterValue and a missing
// VpcId with MissingParameter.
func ValidateCreateRouteTableInput(input *ec2.CreateRouteTableInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreateRouteTable implements the EC2 CreateRouteTable action, creating a route table in a VPC
// via the NATS route table service.
func CreateRouteTable(ctx context.Context, input *ec2.CreateRouteTableInput, natsConn *nats.Conn, accountID string) (ec2.CreateRouteTableOutput, error) {
	var output ec2.CreateRouteTableOutput
	if err := ValidateCreateRouteTableInput(input); err != nil {
		return output, err
	}
	svc := ec2routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.CreateRouteTable(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
