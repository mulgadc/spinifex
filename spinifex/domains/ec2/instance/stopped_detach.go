package instance

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// DetachVolumeFromStopped releases a data volume from a stopped instance's
// record. The volume id and instance id come in as a DetachVolumeInput and the
// device name it held comes back on the attachment.
func (s *InstanceServiceImpl) DetachVolumeFromStopped(ctx context.Context, input *ec2.DetachVolumeInput, accountID string) (*ec2.VolumeAttachment, error) {
	return nil, errors.New("not implemented")
}
