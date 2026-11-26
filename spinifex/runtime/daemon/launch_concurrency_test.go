package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run with -race: two launches of one batch write their VM records while the
// daemon persists every VM, so a field set outside the manager lock is a race.
func TestLaunchRunInstances_ConcurrentLaunchesDoNotRaceWriteState(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	_, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-src",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(t.Context(), ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-src", VolumeID: "vol-src",
	})
	require.NoError(t, err)
	loader := &sysInstAMILoader{ami: ebsmetadata.AMI{ImageID: "ami-race", State: "available", SnapshotID: "snap-src"}, sourceVol: "vol-src"}
	d.instanceService.SetRunInstancesDeps(loader, nil, nil, nil)
	d.instanceService.SetEBSProvider(provider)
	sysInstLaunchFails(d)

	instances := []*vm.VM{
		{ID: "i-0000000000000002a", AccountID: testAccountID, Status: vm.StatePending, Instance: &ec2.Instance{}},
		{ID: "i-0000000000000002b", AccountID: testAccountID, Status: vm.StatePending, Instance: &ec2.Instance{}},
	}
	for _, v := range instances {
		d.vmMgr.Insert(v)
	}

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = d.WriteState()
			}
		}
	}()

	itype := getTestInstanceType(t)
	input := &ec2.RunInstancesInput{ImageId: aws.String("ami-race"), InstanceType: aws.String(itype)}
	d.instanceService.LaunchRunInstances(t.Context(), instances, input, &ec2.InstanceTypeInfo{InstanceType: aws.String(itype)})
	close(stop)
	<-writerDone

	for _, v := range instances {
		d.vmMgr.Inspect(v, func(v *vm.VM) {
			assert.Len(t, v.Instance.BlockDeviceMappings, 1, "the root volume mapping is recorded on %s", v.ID)
		})
	}
}
