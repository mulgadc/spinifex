//test:in-package — it swaps the manager's volume mounter, an unexported dependency.
package instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stoppedAttachAZ = "ap-southeast-2a"

type stoppedAttachFixture struct {
	svc    *InstanceServiceImpl
	stored *vm.VM
	id     string
}

func newStoppedAttachFixture(t *testing.T, volumes ...ebsmetadata.Volume) *stoppedAttachFixture {
	t.Helper()
	id := "i-stopped-attach"
	svc := startVolumesService(t, volumes...)
	stored := startingInstance(id, "vol-root", "vol-data")
	svc.config = &config.Config{AZ: stoppedAttachAZ}
	svc.vmMgr = mgrWith(map[string]*vm.VM{})
	svc.stoppedStore = &vmmock.StateStore{Stopped: map[string]*vm.VM{id: stored}}
	return &stoppedAttachFixture{svc: svc, stored: stored, id: id}
}

func (f *stoppedAttachFixture) attach(volumeID, device string) (*ec2.VolumeAttachment, error) {
	in := &ec2.AttachVolumeInput{InstanceId: aws.String(f.id), VolumeId: aws.String(volumeID)}
	if device != "" {
		in.Device = aws.String(device)
	}
	return f.svc.AttachVolumeToStopped(context.Background(), in, startVolumesAccount)
}

func (f *stoppedAttachFixture) volume(t *testing.T, id string) ebsmetadata.Volume {
	t.Helper()
	v, err := f.svc.metadata.GetVolume(context.Background(), startVolumesAccount, id)
	require.NoError(t, err)
	return v
}

func availableVolume(id string) ebsmetadata.Volume {
	return ebsmetadata.Volume{VolumeID: id, State: "available", AvailabilityZone: stoppedAttachAZ}
}

// AWS lets a volume be attached to a stopped instance; the record then names it
// so the next start mounts it, and the volume is held in-use meanwhile.
func TestAttachVolumeToStopped_AddsTheVolumeAndClaimsIt(t *testing.T) {
	f := newStoppedAttachFixture(t, availableVolume("vol-new"))

	out, err := f.attach("vol-new", "")
	require.NoError(t, err)

	assert.Equal(t, "/dev/sdf", aws.StringValue(out.Device), "the first free data device")
	assert.Equal(t, []string{"vol-root", "vol-data", "vol-new"}, requestedNames(f.stored))
	assert.Equal(t, []string{"vol-root", "vol-data", "vol-new"}, mappedNames(f.stored))
	assert.Equal(t, vm.StateStopped, f.stored.Status)
	got := f.volume(t, "vol-new")
	assert.Equal(t, "in-use", got.State)
	assert.Equal(t, f.id, got.AttachedInstance)
	assert.Equal(t, "/dev/sdf", got.DeviceName)
	for _, r := range f.stored.EBSRequests.Requests {
		if r.Name == "vol-new" {
			assert.False(t, r.Boot, "a data device is not the root")
		}
	}
}

func TestAttachVolumeToStopped_Refusals(t *testing.T) {
	tests := []struct {
		name    string
		volumes []ebsmetadata.Volume
		volume  string
		device  string
		want    string
	}{
		{"volume in use elsewhere", []ebsmetadata.Volume{{VolumeID: "vol-busy", State: "in-use", AttachedInstance: "i-other", AvailabilityZone: stoppedAttachAZ}}, "vol-busy", "", awserrors.ErrorVolumeInUse},
		{"wrong availability zone", []ebsmetadata.Volume{{VolumeID: "vol-far", State: "available", AvailabilityZone: "ap-southeast-2b"}}, "vol-far", "", awserrors.ErrorInvalidVolumeZoneMismatch},
		{"no such volume", nil, "vol-missing", "", awserrors.ErrorInvalidVolumeNotFound},
		{"device already taken", []ebsmetadata.Volume{availableVolume("vol-new")}, "vol-new", "/dev/sdg", awserrors.ErrorInvalidParameterValue},
		{"root device already has a volume", []ebsmetadata.Volume{availableVolume("vol-new")}, "vol-new", "/dev/sda1", awserrors.ErrorInvalidParameterValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newStoppedAttachFixture(t, tt.volumes...)

			_, err := f.attach(tt.volume, tt.device)

			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, tt.want), "got %v, want %s", err, tt.want)
			assert.Equal(t, []string{"vol-root", "vol-data"}, requestedNames(f.stored), "a refused attach changes nothing")
			for _, v := range tt.volumes {
				got := f.volume(t, v.VolumeID)
				assert.Equal(t, v.State, got.State, "a refused attach leaves the volume as it was")
				assert.Equal(t, v.AttachedInstance, got.AttachedInstance)
			}
		})
	}
}

func TestAttachVolumeToStopped_OtherAccountsInstanceIsNotFound(t *testing.T) {
	f := newStoppedAttachFixture(t, availableVolume("vol-new"))

	_, err := f.svc.AttachVolumeToStopped(context.Background(),
		&ec2.AttachVolumeInput{InstanceId: aws.String(f.id), VolumeId: aws.String("vol-new")}, "000000000099")

	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidInstanceIDNotFound))
	assert.Equal(t, []string{"vol-root", "vol-data"}, requestedNames(f.stored))
}

// A record edit that cannot be applied must give the volume back, otherwise it
// stays in-use against an instance that does not name it.
func TestAttachVolumeToStopped_FailedRecordEditReleasesTheVolume(t *testing.T) {
	f := newStoppedAttachFixture(t, availableVolume("vol-new"))
	f.svc.stoppedStore = &vmmock.StateStore{Stopped: map[string]*vm.VM{f.id: f.stored}, UpdateStoppedErr: assert.AnError}

	_, err := f.attach("vol-new", "")

	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorServerInternal))
	got := f.volume(t, "vol-new")
	assert.Equal(t, "available", got.State)
	assert.Empty(t, got.AttachedInstance)
}

// Detaching the root volume of a stopped instance makes it unstartable; the way
// back is attaching a volume at the root device name, which makes it the boot
// volume again so the start finds a root.
func TestAttachVolumeToStopped_RootDeviceNameRestoresTheRootAndTheStartWorks(t *testing.T) {
	id := "i-root-roundtrip"
	stopped := stoppedWithVolumes(startVolumesAccount, id)
	stopped.InstanceType = "t3.micro"
	stopped.Instance.RootDeviceName = aws.String("/dev/sda1")
	f := newStartFixture(t, stopped)
	rec := &mountRecorder{}
	f.svc.vmMgr.SetDeps(vm.Deps{VolumeMounter: rec})
	f.svc.config = &config.Config{AZ: stoppedAttachAZ}
	meta := startVolumesService(t,
		ebsmetadata.Volume{VolumeID: "vol-root", State: "in-use", AttachedInstance: id, AvailabilityZone: stoppedAttachAZ},
		ebsmetadata.Volume{VolumeID: "vol-data", State: "in-use", AttachedInstance: id, AvailabilityZone: stoppedAttachAZ},
		ebsmetadata.Volume{VolumeID: "vol-other", State: "in-use", AttachedInstance: id, AvailabilityZone: stoppedAttachAZ},
	)
	f.svc.metadata = meta.metadata
	stopped.AccountID = startVolumesAccount

	_, err := f.svc.DetachVolumeFromStopped(context.Background(), stoppedDetachInput(id, "vol-root"), startVolumesAccount)
	require.NoError(t, err)
	vol, err := f.svc.metadata.GetVolume(context.Background(), startVolumesAccount, "vol-root")
	require.NoError(t, err)
	vol.State, vol.AttachedInstance = "available", ""
	require.NoError(t, f.svc.metadata.PutVolume(context.Background(), vol))

	out, err := f.svc.AttachVolumeToStopped(context.Background(),
		&ec2.AttachVolumeInput{InstanceId: aws.String(id), VolumeId: aws.String("vol-root"), Device: aws.String("/dev/sda1")}, startVolumesAccount)
	require.NoError(t, err)
	assert.Equal(t, "/dev/sda1", aws.StringValue(out.Device))
	var boot bool
	for _, r := range stopped.EBSRequests.Requests {
		if r.Name == "vol-root" {
			boot = r.Boot
		}
	}
	assert.True(t, boot, "the root device name makes it the boot volume")

	_, err = f.svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: id}, startVolumesAccount)
	require.Error(t, err, "the recorder fails the launch after the mount")
	assert.Contains(t, rec.mounted, "vol-root", "the start now mounts the restored root")
}
