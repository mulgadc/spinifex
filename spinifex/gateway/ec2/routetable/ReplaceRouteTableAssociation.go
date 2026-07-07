package gateway_ec2_routetable

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	"github.com/nats-io/nats.go"
)

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

func ReplaceRouteTableAssociation(ctx context.Context, input *ec2.ReplaceRouteTableAssociationInput, natsConn *nats.Conn, accountID string) (ec2.ReplaceRouteTableAssociationOutput, error) {
	var output ec2.ReplaceRouteTableAssociationOutput
	if err := ValidateReplaceRouteTableAssociationInput(input); err != nil {
		return output, err
	}
	svc := handlers_ec2_routetable.NewNATSRouteTableService(natsConn)
	result, err := svc.ReplaceRouteTableAssociation(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
