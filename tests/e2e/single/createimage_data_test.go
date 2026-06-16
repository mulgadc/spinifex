//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// createImageDataSentinel lives on the root filesystem under /var/lib (not
// /tmp or /var/tmp, which boot-time tmpfiles cleaners may purge) so it is
// captured by the root-volume snapshot and present on a fresh boot.
const createImageDataSentinel = "/var/lib/e2e-createimage-sentinel.bin"

// dataAMIName is distinct from TestCreateImage's "e2e-custom-ami" so this test
// always bakes a *fresh* AMI after the sentinel is written, rather than reusing
// the earlier image (which predates the sentinel).
const dataAMIName = "e2e-data-ami"

// runCreateImageData is the AMI analog of the snapshot-restore test: it proves
// CreateImage captures real root-filesystem bytes by writing a sentinel into
// the singleton's root fs, baking a no-reboot AMI, launching a fresh instance
// from it, and verifying the sentinel's sha256 survives. Sequential; the only
// data-integrity test that costs an extra boot.
func runCreateImageData(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — CreateImage Root Data Fidelity")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	keyName, keyPath := needKeyPair(t, fix)
	instType, _ := needInstanceTypeArch(t, fix)

	host, port := harness.InstancePublicSSHHost(t, inst)
	waitForSSHReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: keyPath}

	// 1. Write a sentinel into the source instance's root filesystem.
	wantSha := harness.GuestWriteFileSentinel(t, tgt, createImageDataSentinel, volumeDataSizeMiB)
	harness.Detail(t, "sentinel", createImageDataSentinel, "sha256", wantSha)
	t.Cleanup(func() {
		_, _ = harness.GuestExec(tgt, "sudo rm -f "+createImageDataSentinel)
	})

	// 2. Bake a no-reboot AMI now that the sentinel is on disk.
	harness.Step(t, "create-image instance=%s name=%s (no-reboot)", instanceID, dataAMIName)
	amiID := ensureCustomAMI(t, fix, instanceID, dataAMIName, "E2E data fidelity image")
	require.NotEmpty(t, amiID, "ensureCustomAMI returned empty ImageId")
	harness.Detail(t, "data_ami", amiID)

	// 3. Launch a throwaway instance from the custom AMI and verify the bytes.
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	harness.Step(t, "run-instances from %s", amiID)
	runOut, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: []*string{aws.String(vpc.SGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "run-instances from custom AMI")
	require.NotEmpty(t, runOut.Instances, "run-instances returned 0 instances")
	newID := aws.StringValue(runOut.Instances[0].InstanceId)
	require.NotEmpty(t, newID, "launched instance has empty InstanceId")
	harness.Detail(t, "throwaway_instance", newID)

	terminated := false
	t.Cleanup(func() {
		if terminated {
			return
		}
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(newID)},
		})
	})

	newInst := harness.WaitForInstanceState(t, fix.AWS, newID, "running")
	newHost, newPort := harness.InstancePublicSSHHost(t, newInst)
	waitForSSHReady(t, newHost, newPort, keyPath)
	newTgt := harness.SSHTarget{User: "ec2-user", Host: newHost, Port: newPort, KeyPath: keyPath}

	gotSha := harness.GuestFileSha(t, newTgt, createImageDataSentinel)
	require.Equalf(t, wantSha, gotSha,
		"sha256 mismatch: sentinel written to source root fs absent/corrupt on AMI-launched instance")
	harness.Detail(t, "ami_data_sha_ok", gotSha)

	harness.Step(t, "terminate-instances %s", newID)
	_, err = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(newID)},
	})
	require.NoError(t, err, "terminate-instances")
	harness.WaitForInstanceState(t, fix.AWS, newID, "terminated")
	terminated = true
}
