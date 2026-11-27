package instance

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// ProjectNamedInstance applies the caller visibility rule to v and projects
// the named facts. A hidden instance is nil.
func ProjectNamedInstance(accountID, instanceID string, v *vm.VM) *projection.NamedInstance {
	if !IsInstanceVisibleToCaller(accountID, v) {
		return nil
	}
	out := &projection.NamedInstance{
		InstanceID: instanceID,
		AccountID:  v.AccountID,
		State:      string(v.Status),
		Deleting:   v.DeletionTimestamp != nil,
	}
	if v.Instance == nil || v.ENIId == "" {
		return out
	}
	for _, nic := range v.Instance.NetworkInterfaces {
		if nic == nil || nic.Attachment == nil || nic.Attachment.DeviceIndex == nil || *nic.Attachment.DeviceIndex != 0 {
			continue
		}
		if aws.StringValue(nic.NetworkInterfaceId) == v.ENIId && aws.StringValue(nic.VpcId) != "" {
			out.PrimaryInterfaceID = v.ENIId
			out.VpcID = aws.StringValue(nic.VpcId)
			out.SubnetID = aws.StringValue(nic.SubnetId)
		}
		break
	}
	return out
}
