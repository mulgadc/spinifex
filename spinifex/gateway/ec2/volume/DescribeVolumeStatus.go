package gateway_ec2_volume

import (
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
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

// DescribeVolumeStatus handles the DescribeVolumeStatus API call
func DescribeVolumeStatus(input *ec2.DescribeVolumeStatusInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVolumeStatusOutput, error) {
	var output ec2.DescribeVolumeStatusOutput

	err := ValidateDescribeVolumeStatusInput(input)
	if err != nil {
		return output, err
	}

	volumeService := handlers_ec2_volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DescribeVolumeStatus(input, accountID)
	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
