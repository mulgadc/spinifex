package instance

import (
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
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
