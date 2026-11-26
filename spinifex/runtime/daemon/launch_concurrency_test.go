package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run with -race: two launches of one batch publish their volumes and build
// their QEMU configuration while the daemon persists every VM, so a field set
// outside the manager lock is a race. QEMU start fails at the block device check.
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
	const devicePath, guestPort = "/dev/spinifex-race-absent", 8080
	d.vmMgr.WaitForBackgroundWork()
	d.vmMgr.SetDeps(vm.Deps{
		NodeID: d.node,
		VolumeMounter: newVolumeMounterAdapter(&stubMounterProvider{export: ebsprovider.VolumeExport{
			Kind: ebsprovider.ExportKindBlock, Block: &ebsprovider.BlockExport{DevicePath: devicePath, SizeBytes: 1 << 30},
		}}, d.node, d.vmMgr),
		VolumeStateUpdater: acceptingVolumeState{},
		InstanceTypes:      newInstanceTypeResolverAdapter(d.resourceMgr),
		NetworkPlumber:     &nopPlumber{},
		DevNetworking:      true,
	})

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
	defer func() {
		close(stop)
		<-writerDone
	}()

	// Several batches, because the race detector sees a race only when no lock
	// orders the two accesses in that run.
	itype := getTestInstanceType(t)
	input := &ec2.RunInstancesInput{ImageId: aws.String("ami-race"), InstanceType: aws.String(itype)}
	for batch := range 3 {
		var instances []*vm.VM
		for i := range 4 {
			id := fmt.Sprintf("i-%017x", 0x2a+batch*4+i)
			v := &vm.VM{
				ID: id, AccountID: testAccountID, Status: vm.StatePending, Instance: &ec2.Instance{}, InstanceType: itype,
				ENIId: "eni-" + id[2:], ENIMac: "02:00:00:00:00:01", HostfwdPorts: []int{guestPort},
			}
			instances = append(instances, v)
			d.vmMgr.Insert(v)
		}
		d.instanceService.LaunchRunInstances(t.Context(), instances, input, &ec2.InstanceTypeInfo{InstanceType: aws.String(itype)})

		for _, v := range instances {
			d.vmMgr.Inspect(v, func(v *vm.VM) {
				assert.Len(t, v.Instance.BlockDeviceMappings, 1, "the root volume mapping is recorded on %s", v.ID)
				assert.NotZero(t, v.HostfwdPortMap[guestPort], "the QEMU configuration was built for %s", v.ID)
				v.EBSRequests.Mu.Lock()
				defer v.EBSRequests.Mu.Unlock()
				require.NotEmpty(t, v.EBSRequests.Requests)
				assert.Equal(t, devicePath, v.EBSRequests.Requests[0].BlockDevice, "the export is recorded on %s", v.ID)
			})
		}
	}
}

type acceptingVolumeState struct{}

func (acceptingVolumeState) UpdateVolumeState(_, _, _, _, _ string) error { return nil }

// nopPlumber is a recordingPlumber that records nothing, so concurrent
// launches can share it.
type nopPlumber struct{ recordingPlumber }

func (*nopPlumber) CleanupTap(string) error { return nil }

// An unmount holds EBSRequests.Mu across its seals, so a Mount that commits its
// exports meanwhile must wait for that lock without holding the manager lock.
func TestVolumeMounterAdapter_CommitExportsWaitsOutsideTheManagerLock(t *testing.T) {
	mgr := vm.NewManager()
	adapter := newVolumeMounterAdapter(&stubMounterProvider{}, "node-commit", mgr)
	instance := &vm.VM{ID: "i-0000000000000003a"}
	instance.EBSRequests.Requests = []vm.EBSRequest{{Name: "vol-commit"}}
	mgr.Insert(instance)

	instance.EBSRequests.Mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			instance.EBSRequests.Mu.Unlock()
		}
	}()
	committed := make(chan struct{})
	go func() {
		adapter.commitExports(instance, []vm.EBSRequest{{Name: "vol-commit", NBDURI: "nbd://commit"}})
		close(committed)
	}()
	assert.Never(t, func() bool {
		select {
		case <-committed:
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 10*time.Millisecond, "the commit did not wait for the requests lock")

	inspected := make(chan struct{})
	go func() {
		mgr.Inspect(instance, func(*vm.VM) {})
		close(inspected)
	}()
	select {
	case <-inspected:
	case <-time.After(2 * time.Second):
		t.Fatal("the manager lock was held while the commit waited for the requests lock")
	}

	instance.EBSRequests.Mu.Unlock()
	unlocked = true
	<-committed
	assert.Equal(t, "nbd://commit", instance.EBSRequests.Requests[0].NBDURI)
}
