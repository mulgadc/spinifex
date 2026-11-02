// Package volume implements the EC2 EBS volume actions: it
// validates each request and forwards it to the volume service over NATS.
package volume

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// volumeIDSuffixPattern is the AWS volume ID shape after vol-: 8 (legacy) or
// 17 lowercase hex characters.
var volumeIDSuffixPattern = regexp.MustCompile(`^([0-9a-f]{8}|[0-9a-f]{17})$`)

// ValidateDescribeVolumesInput accepts a nil input, rejects any VolumeIds entry without the
// vol- prefix with InvalidVolumeID.Malformed, and any with a malformed suffix with
// InvalidParameterValue, as AWS does.
func ValidateDescribeVolumesInput(input *ec2.DescribeVolumesInput) error {
	if input == nil {
		return nil
	}

	if input.VolumeIds != nil {
		for _, volumeId := range input.VolumeIds {
			if volumeId == nil {
				continue
			}
			suffix, ok := strings.CutPrefix(*volumeId, "vol-")
			if !ok {
				return errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
			}
			if !volumeIDSuffixPattern.MatchString(suffix) {
				return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
					"Value (%s) for parameter volumes is invalid. Expected: 'vol-...'.", *volumeId)
			}
		}
	}

	return nil
}

// DescribeVolumes handles the DescribeVolumes API call.
func DescribeVolumes(ctx context.Context, input *ec2.DescribeVolumesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVolumesOutput, error) {
	var output ec2.DescribeVolumesOutput

	volumeService := ec2volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DescribeVolumes(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
