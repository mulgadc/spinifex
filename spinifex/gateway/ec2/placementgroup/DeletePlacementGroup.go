package gateway_ec2_placementgroup

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/nats-io/nats.go"
)

func ValidateDeletePlacementGroupInput(input *ec2.DeletePlacementGroupInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.GroupName == nil || *input.GroupName == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DeletePlacementGroup handles the EC2 DeletePlacementGroup API call.
func DeletePlacementGroup(ctx context.Context, input *ec2.DeletePlacementGroupInput, natsConn *nats.Conn, accountID string) (ec2.DeletePlacementGroupOutput, error) {
	var output ec2.DeletePlacementGroupOutput

	if err := ValidateDeletePlacementGroupInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)
	result, err := svc.DeletePlacementGroup(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
