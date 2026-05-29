package gateway_ec2_volume

import (
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	"github.com/nats-io/nats.go"
)

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

// ModifyVolume handles the ModifyVolume API call
func ModifyVolume(input *ec2.ModifyVolumeInput, natsConn *nats.Conn, accountID string) (ec2.ModifyVolumeOutput, error) {
	var output ec2.ModifyVolumeOutput

	err := ValidateModifyVolumeInput(input)
	if err != nil {
		return output, err
	}

	volumeService := handlers_ec2_volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.ModifyVolume(input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
