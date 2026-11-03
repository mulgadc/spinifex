package instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const startVolumesAccount = "000000000011"

func startVolumesService(t *testing.T, volumes ...ebsmetadata.Volume) *InstanceServiceImpl {
	t.Helper()
	store := objectstore.NewMemoryObjectStore()
	meta := ebsmetadata.NewStore(store, testRootBucket)
	for _, v := range volumes {
		v.TenantID = startVolumesAccount
		require.NoError(t, meta.PutVolume(context.Background(), v))
	}
	return &InstanceServiceImpl{metadata: meta}
}

func startingInstance(id string, names ...string) *vm.VM {
	v := &vm.VM{ID: id, AccountID: startVolumesAccount, Status: vm.StateStopped, Instance: &ec2.Instance{InstanceId: aws.String(id)}}
	for i, n := range names {
		boot := i == 0
		device := "/dev/sd" + string(rune('f'+i))
		if boot {
			device = "/dev/sda1"
		}
		v.EBSRequests.Requests = append(v.EBSRequests.Requests, viperblocklegacyv1.EBSRequest{Name: n, Boot: boot, DeviceName: device})
		v.Instance.BlockDeviceMappings = append(v.Instance.BlockDeviceMappings, &ec2.InstanceBlockDeviceMapping{
			DeviceName: aws.String(device), Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String(n)},
		})
	}
	return v
}

func requestedNames(v *vm.VM) []string {
	var out []string
	for _, r := range v.EBSRequests.Requests {
		out = append(out, r.Name)
	}
	return out
}

func mappedNames(v *vm.VM) []string {
	var out []string
	for _, m := range v.Instance.BlockDeviceMappings {
		out = append(out, aws.StringValue(m.Ebs.VolumeId))
	}
	return out
}

// An instance stopped on an older build had its data volumes released, and they
// may since have been attached elsewhere or deleted; starting must not take a
// volume that is no longer its own.
func TestReconcileStartVolumes_DropsVolumesThatAreNoLongerTheInstances(t *testing.T) {
	svc := startVolumesService(t,
		ebsmetadata.Volume{VolumeID: "vol-root", State: "in-use", AttachedInstance: "i-old"},
		ebsmetadata.Volume{VolumeID: "vol-elsewhere", State: "in-use", AttachedInstance: "i-someone-else"},
		ebsmetadata.Volume{VolumeID: "vol-free", State: "available"},
		ebsmetadata.Volume{VolumeID: "vol-mine", State: "in-use", AttachedInstance: "i-old"},
	)
	inst := startingInstance("i-old", "vol-root", "vol-elsewhere", "vol-gone", "vol-free", "vol-mine")

	require.NoError(t, svc.reconcileStartVolumes(context.Background(), inst))

	want := []string{"vol-root", "vol-free", "vol-mine"}
	assert.Equal(t, want, requestedNames(inst),
		"attached-elsewhere and deleted volumes are dropped; free and own ones stay")
	assert.Equal(t, want, mappedNames(inst), "the API view stops listing them too")
}

func TestReconcileStartVolumes_MissingBootVolumeFailsTheStart(t *testing.T) {
	svc := startVolumesService(t, ebsmetadata.Volume{VolumeID: "vol-data", State: "available"})
	inst := startingInstance("i-old", "vol-root", "vol-data")
	inst.Instance.RootDeviceName = aws.String("/dev/sda1")

	err := svc.reconcileStartVolumes(context.Background(), inst)

	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterValue), "got %v", err)
	assert.Contains(t, err.Error(), "Instance does not have a volume attached at root (/dev/sda1)")
}

// A root volume detached while the instance was stopped leaves no boot request
// in the record; AWS refuses the start with the same answer.
func TestReconcileStartVolumes_NoRootVolumeAttachedFailsTheStart(t *testing.T) {
	svc := startVolumesService(t, ebsmetadata.Volume{VolumeID: "vol-data", State: "in-use", AttachedInstance: "i-old"})
	inst := startingInstance("i-old", "vol-root", "vol-data")
	require.True(t, removeVolumeFromRecord(inst, "vol-root"))
	inst.Instance.RootDeviceName = aws.String("/dev/sda1")

	err := svc.reconcileStartVolumes(context.Background(), inst)

	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidParameterValue), "got %v", err)
	assert.Contains(t, err.Error(), "Instance does not have a volume attached at root (/dev/sda1)")
	assert.Equal(t, []string{"vol-data"}, requestedNames(inst), "a refused start changes nothing")
}

// An instance stopped on an older build had its data volumes released to
// available with no attachment; those are still the instance's own and a
// start must bring them back.
func TestReconcileStartVolumes_ReattachesStillAvailableUnattachedVolume(t *testing.T) {
	svc := startVolumesService(t,
		ebsmetadata.Volume{VolumeID: "vol-root", State: "in-use", AttachedInstance: "i-old"},
		ebsmetadata.Volume{VolumeID: "vol-released", State: "available"},
	)
	inst := startingInstance("i-old", "vol-root", "vol-released")

	require.NoError(t, svc.reconcileStartVolumes(context.Background(), inst))

	assert.Equal(t, []string{"vol-root", "vol-released"}, requestedNames(inst))
	assert.Equal(t, []string{"vol-root", "vol-released"}, mappedNames(inst))
}
