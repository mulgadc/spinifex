package instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stoppedWithVolumes(owner, id string) *vm.VM {
	v := &vm.VM{
		ID: id, AccountID: owner, Status: vm.StateStopped,
		Instance: &ec2.Instance{
			InstanceId: aws.String(id),
			BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
				{DeviceName: aws.String("/dev/sda1"), Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String("vol-root")}},
				{DeviceName: aws.String("/dev/sdf"), Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String("vol-data")}},
				{DeviceName: aws.String("/dev/sdg"), Ebs: &ec2.EbsInstanceBlockDevice{VolumeId: aws.String("vol-other")}},
			},
		},
	}
	v.EBSRequests.Requests = []viperblocklegacyv1.EBSRequest{
		{Name: "vol-root", Boot: true, DeviceName: "/dev/sda1"},
		{Name: "vol-data", DeviceName: "/dev/sdf"},
		{Name: "vol-other", DeviceName: "/dev/sdg"},
	}
	return v
}

func stoppedDetachInput(instanceID, volumeID string) *ec2.DetachVolumeInput {
	return &ec2.DetachVolumeInput{InstanceId: aws.String(instanceID), VolumeId: aws.String(volumeID)}
}

func volumeNames(v *vm.VM) []string {
	var out []string
	for _, r := range v.EBSRequests.Requests {
		out = append(out, r.Name)
	}
	return out
}

// AWS lets a stopped instance's data volume be detached; the record must stop
// claiming it so a later start does not re-attach it.
func TestDetachVolumeFromStopped_RemovesOnlyThatVolume(t *testing.T) {
	owner := awsidentifiers.GlobalAccountID
	id := "i-stopped-detach"
	stored := stoppedWithVolumes(owner, id)
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{id: stored}}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{}), stoppedStore: store}

	out, err := svc.DetachVolumeFromStopped(context.Background(), stoppedDetachInput(id, "vol-data"), owner)
	require.NoError(t, err)

	assert.Equal(t, "/dev/sdf", aws.StringValue(out.Device))
	assert.Equal(t, id, aws.StringValue(out.InstanceId))
	assert.Equal(t, []string{"vol-root", "vol-other"}, volumeNames(stored))
	var mapped []string
	for _, m := range stored.Instance.BlockDeviceMappings {
		mapped = append(mapped, aws.StringValue(m.Ebs.VolumeId))
	}
	assert.Equal(t, []string{"vol-root", "vol-other"}, mapped)
	assert.Equal(t, vm.StateStopped, stored.Status, "the instance stays stopped")
}

func TestDetachVolumeFromStopped_Refusals(t *testing.T) {
	owner := awsidentifiers.GlobalAccountID
	tests := []struct {
		name    string
		caller  string
		volume  string
		stopped bool
		want    string
	}{
		{"boot volume", owner, "vol-root", true, awserrors.ErrorOperationNotPermitted},
		{"volume not on the instance", owner, "vol-elsewhere", true, awserrors.ErrorIncorrectState},
		{"another account", "000000000099", "vol-data", true, awserrors.ErrorInvalidInstanceIDNotFound},
		{"no such stopped instance", owner, "vol-data", false, awserrors.ErrorInvalidInstanceIDNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := "i-stopped-refuse"
			stored := stoppedWithVolumes(owner, id)
			store := &vmmock.StateStore{Stopped: map[string]*vm.VM{}}
			if tt.stopped {
				store.Stopped[id] = stored
			}
			svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{}), stoppedStore: store}

			_, err := svc.DetachVolumeFromStopped(context.Background(), stoppedDetachInput(id, tt.volume), tt.caller)

			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, tt.want), "got %v, want %s", err, tt.want)
			assert.Equal(t, []string{"vol-root", "vol-data", "vol-other"}, volumeNames(stored),
				"a refused detach changes nothing")
		})
	}
}
