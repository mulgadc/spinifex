package routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDisassociateRouteTableInput rejects a nil input with InvalidParameterValue and a
// missing AssociationId with MissingParameter.
func ValidateDisassociateRouteTableInput(input *ec2.DisassociateRouteTableInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.AssociationId == nil || *input.AssociationId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DisassociateRouteTable implements the EC2 DisassociateRouteTable action, removing a subnet
// association via the NATS route table service.
func DisassociateRouteTable(ctx context.Context, input *ec2.DisassociateRouteTableInput, natsConn *nats.Conn, accountID string) (ec2.DisassociateRouteTableOutput, error) {
	var output ec2.DisassociateRouteTableOutput
	if err := ValidateDisassociateRouteTableInput(input); err != nil {
		return output, err
	}
	svc := ec2routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.DisassociateRouteTable(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
