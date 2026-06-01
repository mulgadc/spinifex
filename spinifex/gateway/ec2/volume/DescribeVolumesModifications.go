package gateway_ec2_volume

import (
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	"github.com/nats-io/nats.go"
)

func ValidateDescribeVolumesModificationsInput(input *ec2.DescribeVolumesModificationsInput) error {
	if input == nil {
		return nil
	}

	for _, volumeId := range input.VolumeIds {
		if volumeId != nil && !strings.HasPrefix(*volumeId, "vol-") {
			return errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
		}
	}

	return nil
}

// DescribeVolumesModifications handles the DescribeVolumesModifications API call
func DescribeVolumesModifications(input *ec2.DescribeVolumesModificationsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVolumesModificationsOutput, error) {
	var output ec2.DescribeVolumesModificationsOutput

	err := ValidateDescribeVolumesModificationsInput(input)
	if err != nil {
		return output, err
	}

	volumeService := handlers_ec2_volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DescribeVolumesModifications(input, accountID)
	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
