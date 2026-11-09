package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/require"
)

// bootVolumeID is the root volume a fixture instance is given.
func bootVolumeID(instanceID string) string { return "vol-root-" + instanceID }

// withBootVolume gives an instance fixture the root volume every real instance
// has: a boot request at the root device and the matching API mapping.
func withBootVolume(v *vm.VM) *vm.VM {
	id := bootVolumeID(v.ID)
	v.EBSRequests.Requests = append(v.EBSRequests.Requests, vm.EBSRequest{Name: id, Boot: true, DeviceName: ebsmetadata.RootDeviceName})
	if v.Instance != nil {
		v.Instance.BlockDeviceMappings = append(v.Instance.BlockDeviceMappings, &ec2.InstanceBlockDeviceMapping{
			DeviceName: aws.String(ebsmetadata.RootDeviceName),
			Ebs:        &ec2.EbsInstanceBlockDevice{VolumeId: aws.String(id)},
		})
	}
	return v
}

// seedBootVolume registers the root volume document of an instance fixture, so
// a start finds the volume its record names.
func seedBootVolume(t *testing.T, store objectstore.ObjectStore, bucket, accountID, instanceID string) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, bucket).PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID:         bootVolumeID(instanceID),
		TenantID:         accountID,
		State:            "in-use",
		AttachedInstance: instanceID,
	}))
}
