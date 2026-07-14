package gateway_ec2_volume

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	"github.com/nats-io/nats.go"
)

func ValidateCreateVolumeInput(input *ec2.CreateVolumeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Size may be omitted when restoring from a snapshot; the handler
	// defaults to the snapshot size.
	hasSnapshot := input.SnapshotId != nil && *input.SnapshotId != ""
	if input.Size == nil && !hasSnapshot {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Size != nil && (*input.Size < 1 || *input.Size > 16384) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.AvailabilityZone == nil || *input.AvailabilityZone == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.VolumeType != nil && *input.VolumeType != "" && *input.VolumeType != "gp3" {
		return errors.New(awserrors.ErrorUnknownVolumeType)
	}

	return nil
}

// CreateVolume handles the CreateVolume API call
func CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, natsConn *nats.Conn, accountID string) (ec2.Volume, error) {
	var output ec2.Volume

	err := ValidateCreateVolumeInput(input)
	if err != nil {
		return output, err
	}

	volumeService := handlers_ec2_volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.CreateVolume(ctx, input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
