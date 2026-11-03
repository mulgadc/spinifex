package instance

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// DetachVolumeFromStopped removes a data volume from a stopped instance's
// record, so a later start no longer attaches it. It does not touch the
// volume's own state; the caller releases that once the record is clean. The
// device the volume held comes back on the attachment.
func (s *InstanceServiceImpl) DetachVolumeFromStopped(ctx context.Context, input *ec2.DetachVolumeInput, accountID string) (*ec2.VolumeAttachment, error) {
	if input == nil || aws.StringValue(input.InstanceId) == "" || aws.StringValue(input.VolumeId) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if s.stoppedStore == nil {
		slog.ErrorContext(ctx, "DetachVolumeFromStopped: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	instanceID, volumeID := *input.InstanceId, *input.VolumeId

	stored, err := s.stoppedStore.LoadStoppedInstance(instanceID)
	if err != nil {
		slog.ErrorContext(ctx, "DetachVolumeFromStopped: failed to load stopped instance", "instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if stored == nil || !IsInstanceVisible(accountID, stored.AccountID) {
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	req, ok := findVolumeRequest(stored, volumeID)
	if !ok {
		return nil, errors.New(awserrors.ErrorIncorrectState)
	}
	if req.Boot || req.EFI {
		return nil, errors.New(awserrors.ErrorOperationNotPermitted)
	}

	var removed bool
	_, err = s.stoppedStore.UpdateStoppedInstance(instanceID, func(v *vm.VM) {
		removed = removeVolumeFromRecord(v, volumeID)
	})
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			slog.WarnContext(ctx, "DetachVolumeFromStopped: instance claimed concurrently", "instanceId", instanceID)
			return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
		}
		slog.ErrorContext(ctx, "DetachVolumeFromStopped: failed to update stopped instance", "instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if !removed {
		return nil, errors.New(awserrors.ErrorIncorrectState)
	}

	return &ec2.VolumeAttachment{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String(req.DeviceName),
		State:      aws.String(ec2.VolumeAttachmentStateDetaching),
	}, nil
}

func findVolumeRequest(v *vm.VM, volumeID string) (viperblocklegacyv1.EBSRequest, bool) {
	v.EBSRequests.Mu.Lock()
	defer v.EBSRequests.Mu.Unlock()
	i := slices.IndexFunc(v.EBSRequests.Requests, func(r viperblocklegacyv1.EBSRequest) bool { return r.Name == volumeID })
	if i < 0 {
		return viperblocklegacyv1.EBSRequest{}, false
	}
	return v.EBSRequests.Requests[i], true
}

// removeVolumeFromRecord drops the volume from the mount list and the API view
// of the record, and reports whether it was a data volume that was there.
func removeVolumeFromRecord(v *vm.VM, volumeID string) bool {
	v.EBSRequests.Mu.Lock()
	defer v.EBSRequests.Mu.Unlock()
	i := slices.IndexFunc(v.EBSRequests.Requests, func(r viperblocklegacyv1.EBSRequest) bool { return r.Name == volumeID })
	if i < 0 || v.EBSRequests.Requests[i].Boot || v.EBSRequests.Requests[i].EFI {
		return false
	}
	v.EBSRequests.Requests = slices.Delete(v.EBSRequests.Requests, i, i+1)
	if v.Instance != nil {
		v.Instance.BlockDeviceMappings = slices.DeleteFunc(v.Instance.BlockDeviceMappings, func(m *ec2.InstanceBlockDeviceMapping) bool {
			return m != nil && m.Ebs != nil && aws.StringValue(m.Ebs.VolumeId) == volumeID
		})
	}
	return true
}
