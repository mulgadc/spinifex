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

func ValidateDescribeVolumeStatusInput(input *ec2.DescribeVolumeStatusInput) error {
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

// DescribeVolumeStatus handles the DescribeVolumeStatus API call.
func DescribeVolumeStatus(ctx context.Context, input *ec2.DescribeVolumeStatusInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVolumeStatusOutput, error) {
	var output ec2.DescribeVolumeStatusOutput

	err := ValidateDescribeVolumeStatusInput(input)
	if err != nil {
		return output, err
	}

	volumeService := ec2volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DescribeVolumeStatus(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
