package instance

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

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
	if i < 0 || v.EBSRequests.Requests[i].EFI {
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

// stoppedAttachDevice picks the device a volume takes on a stopped instance and
// reports whether it is the root. An empty request takes the next free
// /dev/sd[f-p]; a named device must be free, and the root device name is free
// only while no boot volume is attached.
func stoppedAttachDevice(v *vm.VM, requested string) (string, bool, error) {
	root := ebsmetadata.RootDeviceName
	if v.Instance != nil && aws.StringValue(v.Instance.RootDeviceName) != "" {
		root = aws.StringValue(v.Instance.RootDeviceName)
	}

	v.EBSRequests.Mu.Lock()
	defer v.EBSRequests.Mu.Unlock()
	used := make(map[string]bool, len(v.EBSRequests.Requests))
	hasBoot := false
	for _, r := range v.EBSRequests.Requests {
		if r.DeviceName != "" {
			used[r.DeviceName] = true
		}
		hasBoot = hasBoot || r.Boot
	}
	if v.Instance != nil {
		for _, m := range v.Instance.BlockDeviceMappings {
			if m != nil && m.DeviceName != nil {
				used[*m.DeviceName] = true
			}
		}
	}

	if requested == "" {
		for c := 'f'; c <= 'p'; c++ {
			if dev := fmt.Sprintf("/dev/sd%c", c); !used[dev] {
				return dev, false, nil
			}
		}
		return "", false, errors.New(awserrors.ErrorAttachmentLimitExceeded)
	}
	if used[requested] {
		return "", false, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	boot := requested == root
	if boot && hasBoot {
		return "", false, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return requested, boot, nil
}

// addVolumeToRecord adds the volume to the mount list and the API view of the
// record. The root device name makes it the boot volume.
func addVolumeToRecord(v *vm.VM, volumeID, device string, boot bool) {
	v.EBSRequests.Mu.Lock()
	defer v.EBSRequests.Mu.Unlock()
	v.EBSRequests.Requests = append(v.EBSRequests.Requests, viperblocklegacyv1.EBSRequest{
		Name:       volumeID,
		DeviceName: device,
		Boot:       boot,
	})
	if v.Instance == nil {
		return
	}
	mapping := &ec2.InstanceBlockDeviceMapping{}
	mapping.SetDeviceName(device)
	mapping.Ebs = &ec2.EbsInstanceBlockDevice{}
	mapping.Ebs.SetVolumeId(volumeID)
	mapping.Ebs.SetAttachTime(time.Now())
	mapping.Ebs.SetDeleteOnTermination(false)
	mapping.Ebs.SetStatus("attached")
	v.Instance.BlockDeviceMappings = append(v.Instance.BlockDeviceMappings, mapping)
}
