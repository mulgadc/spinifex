package volume

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateModifyVolumeInput requires a vol- prefixed VolumeId (InvalidVolumeID.Malformed) and
// rejects a non-positive Size with InvalidParameterValue.
func ValidateModifyVolumeInput(input *ec2.ModifyVolumeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.VolumeId == nil || !strings.HasPrefix(*input.VolumeId, "vol-") {
		return errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
	}

	if input.Size != nil && *input.Size <= 0 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return nil
}

// ModifyVolume handles the ModifyVolume API call.
func ModifyVolume(ctx context.Context, input *ec2.ModifyVolumeInput, natsConn *nats.Conn, accountID string) (ec2.ModifyVolumeOutput, error) {
	var output ec2.ModifyVolumeOutput

	err := ValidateModifyVolumeInput(input)
	if err != nil {
		return output, err
	}

	volumeService := ec2volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.ModifyVolume(ctx, input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
