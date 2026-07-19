package gateway_ec2_volume

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	"github.com/nats-io/nats.go"
)

func ValidateDescribeVolumesInput(input *ec2.DescribeVolumesInput) error {
	if input == nil {
		return nil
	}

	if input.VolumeIds != nil {
		for _, volumeId := range input.VolumeIds {
			if volumeId != nil && !strings.HasPrefix(*volumeId, "vol-") {
				return errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
			}
		}
	}

	return nil
}

// DescribeVolumes handles the DescribeVolumes API call.
func DescribeVolumes(ctx context.Context, input *ec2.DescribeVolumesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVolumesOutput, error) {
	var output ec2.DescribeVolumesOutput

	err := ValidateDescribeVolumesInput(input)
	if err != nil {
		return output, err
	}

	volumeService := handlers_ec2_volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DescribeVolumes(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
