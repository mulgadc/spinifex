package gateway_ec2_placementgroup

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/nats-io/nats.go"
)

func ValidateCreatePlacementGroupInput(input *ec2.CreatePlacementGroupInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.GroupName == nil || *input.GroupName == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Strategy == nil || *input.Strategy == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// CreatePlacementGroup handles the EC2 CreatePlacementGroup API call.
func CreatePlacementGroup(input *ec2.CreatePlacementGroupInput, natsConn *nats.Conn, accountID string) (ec2.CreatePlacementGroupOutput, error) {
	var output ec2.CreatePlacementGroupOutput

	if err := ValidateCreatePlacementGroupInput(input); err != nil {
		return output, err
	}

	svc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)
	result, err := svc.CreatePlacementGroup(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
