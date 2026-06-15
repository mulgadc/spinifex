//go:build e2e

package gpu

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// TestGPUPassthrough_Launch verifies that RunInstances with a GPU instance
// type succeeds and reaches running state, confirming the VFIO bind path.
func TestGPUPassthrough_Launch(t *testing.T) {
	fix := requireGPUFixture(t)
	harness.Phase(t, "GPU — Launch: RunInstances with %s", fix.GPUInstanceType)

	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	keyName, _ := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))

	instanceID := launchGPUInstance(t, fix, vpc.SubnetID, []string{vpc.SGID}, keyName)
	harness.Detail(t, "instance", instanceID, "type", fix.GPUInstanceType, "ami", fix.AMIID)

	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	assert.Equal(t, "running", aws.StringValue(inst.State.Name))
}

// TestGPUPassthrough_DeviceVisible SSHes into the GPU VM and confirms the
// NVIDIA PCI device is visible via sysfs, proving the vfio-pci passthrough
// wired the device into the guest.
func TestGPUPassthrough_DeviceVisible(t *testing.T) {
	fix := requireGPUFixture(t)
	harness.Phase(t, "GPU — DeviceVisible: NVIDIA PCI device present in VM sysfs")

	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	instanceID := launchGPUInstance(t, fix, vpc.SubnetID, []string{vpc.SGID}, keyName)
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	harness.Detail(t, "instance", instanceID)

	host, port := harness.InstancePublicSSHHost(t, inst)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	harness.Step(t, "waiting for SSH on %s:%d", host, port)
	waitForSSH(t, tgt, 5*time.Minute)

	harness.Step(t, "checking sysfs for NVIDIA vendor (0x10de)")
	out := sshRun(t, tgt, "grep -l 0x10de /sys/bus/pci/devices/*/vendor 2>/dev/null | wc -l")
	assert.NotEqual(t, "0", out, "no NVIDIA PCI device found in VM sysfs — GPU passthrough did not wire the device into the guest")
}

// TestGPUPassthrough_DriverAccess confirms nvidia-smi succeeds inside the VM,
// proving the guest OS can communicate with the passed-through GPU.
func TestGPUPassthrough_DriverAccess(t *testing.T) {
	fix := requireGPUFixture(t)
	harness.Phase(t, "GPU — DriverAccess: nvidia-smi succeeds in VM")

	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	instanceID := launchGPUInstance(t, fix, vpc.SubnetID, []string{vpc.SGID}, keyName)
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	harness.Detail(t, "instance", instanceID)

	host, port := harness.InstancePublicSSHHost(t, inst)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	harness.Step(t, "waiting for SSH on %s:%d", host, port)
	waitForSSH(t, tgt, 5*time.Minute)

	harness.Step(t, "running nvidia-smi")
	out := sshRun(t, tgt, "nvidia-smi --query-gpu=name --format=csv,noheader")
	assert.NotEmpty(t, out, "nvidia-smi returned no GPU name — driver not loaded or GPU not accessible")
	harness.Detail(t, "gpu_name", out)
}

// TestGPUPassthrough_Release terminates a GPU instance and proves the GPU
// returns to the pool by launching a second instance with the same type.
// A hung VFIO release would cause the second RunInstances to fail with an
// insufficient-capacity error.
func TestGPUPassthrough_Release(t *testing.T) {
	fix := requireGPUFixture(t)
	harness.Phase(t, "GPU — Release: GPU returns to pool after termination")

	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	keyName, _ := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))
	sgIDs := []*string{aws.String(vpc.SGID)}

	harness.Step(t, "launching first GPU instance")
	out1, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(fix.AMIID),
		InstanceType:     aws.String(fix.GPUInstanceType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: sgIDs,
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "RunInstances first GPU instance")
	require.NotEmpty(t, out1.Instances)
	id1 := aws.StringValue(out1.Instances[0].InstanceId)
	harness.Detail(t, "instance1", id1)

	harness.WaitForInstanceState(t, fix.AWS, id1, "running")

	harness.Step(t, "terminating first instance — exercises gpuManager.Release()")
	_, err = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(id1)},
	})
	require.NoError(t, err, "TerminateInstances")
	harness.WaitForInstanceState(t, fix.AWS, id1, "terminated")

	harness.Step(t, "launching second GPU instance to prove pool was released")
	out2, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(fix.AMIID),
		InstanceType:     aws.String(fix.GPUInstanceType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: sgIDs,
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "RunInstances second GPU instance — GPU was not returned to pool after termination")
	require.NotEmpty(t, out2.Instances)
	id2 := aws.StringValue(out2.Instances[0].InstanceId)
	harness.Detail(t, "instance2", id2)
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(id2)},
		})
		harness.WaitForInstanceState(t, fix.AWS, id2, "terminated")
	})
	harness.WaitForInstanceState(t, fix.AWS, id2, "running")
}
