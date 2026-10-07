package routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteRouteTableInput rejects a nil input with InvalidParameterValue and a missing
// RouteTableId with MissingParameter.
func ValidateDeleteRouteTableInput(input *ec2.DeleteRouteTableInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeleteRouteTable implements the EC2 DeleteRouteTable action via the NATS route table service.
func DeleteRouteTable(ctx context.Context, input *ec2.DeleteRouteTableInput, natsConn *nats.Conn, accountID string) (ec2.DeleteRouteTableOutput, error) {
	var output ec2.DeleteRouteTableOutput
	if err := ValidateDeleteRouteTableInput(input); err != nil {
		return output, err
	}
	svc := ec2routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.DeleteRouteTable(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
