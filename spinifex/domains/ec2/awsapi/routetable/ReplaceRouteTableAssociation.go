package routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateReplaceRouteTableAssociationInput rejects a nil input with InvalidParameterValue and a
// missing AssociationId or RouteTableId with MissingParameter.
func ValidateReplaceRouteTableAssociationInput(input *ec2.ReplaceRouteTableAssociationInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.AssociationId == nil || *input.AssociationId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RouteTableId == nil || *input.RouteTableId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// ReplaceRouteTableAssociation implements the EC2 ReplaceRouteTableAssociation action, moving an
// existing association to a different route table via the NATS route table service.
func ReplaceRouteTableAssociation(ctx context.Context, input *ec2.ReplaceRouteTableAssociationInput, natsConn *nats.Conn, accountID string) (ec2.ReplaceRouteTableAssociationOutput, error) {
	var output ec2.ReplaceRouteTableAssociationOutput
	if err := ValidateReplaceRouteTableAssociationInput(input); err != nil {
		return output, err
	}
	svc := ec2routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.ReplaceRouteTableAssociation(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
