package gateway_ec2_volume

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

	volumeService := ec2volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DescribeVolumes(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
