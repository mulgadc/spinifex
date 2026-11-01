package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedMicroVMImage points microvmDir at a temp tree holding the shared
// firmware and the load balancer's kernel and initramfs.
func sharedMicroVMImage(t *testing.T) {
	t.Helper()
	orig := microvmDir
	microvmDir = t.TempDir()
	t.Cleanup(func() { microvmDir = orig })
	for _, n := range []string{vm.MicroVMFirmware, "vmlinuz", "initramfs.cpio.gz"} {
		require.NoError(t, os.WriteFile(filepath.Join(microvmDir, n), []byte("x"), 0o644))
	}
}

// sqsTestInstanceType is the smallest of the daemon's types above the in-VM
// store's heap limit, so a fixture never lands on an undersized pick.
func sqsTestInstanceType(t *testing.T, d *Daemon) string {
	t.Helper()
	best, mem := "", int64(0)
	for name, it := range d.resourceMgr.instanceTypes {
		m := instanceTypeMemoryMiB(it)
		if m > sqsStoreMinMemoryMiB && (best == "" || m < mem || (m == mem && name < best)) {
			best, mem = name, m
		}
	}
	require.NotEmpty(t, best, "no instance type fits the SQS guest")
	return best
}

func (f *sysInstFixture) systemVolume(t *testing.T) string {
	t.Helper()
	out, err := f.d.volumeService.CreateVolume(t.Context(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(f.d.config.AZ),
		Size:             aws.Int64(1),
		VolumeType:       aws.String("gp3"),
	}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.VolumeId)
}

func (f *sysInstFixture) volumeState(t *testing.T, volumeID string) string {
	t.Helper()
	out, err := f.d.volumeService.DescribeVolumes(t.Context(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(volumeID)},
	}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1)
	return aws.StringValue(out.Volumes[0].State)
}

// sysInstNoSideEffects asserts a refused launch reserved no capacity, recorded
// no VM and left its ENI unattached.
func (f *sysInstFixture) sysInstNoSideEffects(t *testing.T, eni *ec2.NetworkInterface, vcpuBefore, vmsBefore int) {
	t.Helper()
	f.d.resourceMgr.mu.Lock()
	vcpu := f.d.resourceMgr.allocatedVCPU
	f.d.resourceMgr.mu.Unlock()
	assert.Equal(t, vcpuBefore, vcpu, "no capacity may be reserved")
	assert.Equal(t, vmsBefore, f.d.vmMgr.Count(), "no VM may be recorded")
	rec := f.eniRecord(t, testAccountID, aws.StringValue(eni.NetworkInterfaceId))
	assert.Empty(t, rec.InstanceId, "the ENI must stay unattached")
}

func (f *sysInstFixture) capacity() int {
	f.d.resourceMgr.mu.Lock()
	defer f.d.resourceMgr.mu.Unlock()
	return f.d.resourceMgr.allocatedVCPU
}

func TestLaunchSystemInstance_LoadBalancerUnchangedAndRefusesOtherKinds(t *testing.T) {
	f := sqsWorkerFixture(t, true)

	var launched *vm.VM
	f.d.vmMgr.WaitForBackgroundWork()
	f.d.vmMgr.SetDeps(vm.Deps{
		NodeID: f.d.node,
		VolumeMounter: &hookVolumeMounter{onMount: func(v *vm.VM) {
			launched = v
			f.d.vmMgr.UpdateState(v.ID, func(x *vm.VM) { x.Status = vm.StateRunning })
		}},
		VolumeStateUpdater: f.d.volumeService,
	})
	_, err := f.d.LaunchSystemInstance(f.input(f.newENI(t)))
	require.NoError(t, err)
	require.NotNil(t, launched)
	assert.Equal(t, tags.ManagedByELBv2, launched.ManagedBy)
	assert.Empty(t, launched.EBSRequests.Requests)
	assert.Equal(t, filepath.Join(microvmDir, "vmlinuz"), launched.Config.KernelImage)
	names := make([]string, 0, len(launched.Config.FwCfg))
	for _, e := range launched.Config.FwCfg {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"opt/spinifex/netcfg", "opt/spinifex/lb-agent-env", "opt/spinifex/ca-cert"}, names)

	// The load balancer path would boot the load balancer image, so it refuses
	// the SQS kind before reserving anything.
	eni := f.newENI(t)
	vcpu, vms := f.capacity(), f.d.vmMgr.Count()
	_, err = f.d.LaunchSystemInstance(f.workerInput(t, eni, f.systemVolume(t)))
	require.ErrorIs(t, err, systeminstance.ErrNonLoadBalancerKind)
	f.sysInstNoSideEffects(t, eni, vcpu, vms)
}

// An SQS store VM recorded by an earlier release is refused on relaunch with
// its data volume kept: this release runs no separate store.
func TestRefreshSystemInstanceState_RefusesAnEarlierSQSStoreVM(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	vol := f.systemVolume(t)
	inst := &vm.VM{ID: "i-0123456789abcdef0", ManagedBy: tags.ManagedBySQS}
	inst.SQSStoreGuest = &vm.SQSStoreGuest{NodeID: f.d.node, DataVolumeID: vol}

	err := f.d.refreshSystemInstanceState(inst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not relaunched; its data volume is kept")
	assert.Equal(t, "available", f.volumeState(t, vol), "the volume is neither attached nor deleted")
}
