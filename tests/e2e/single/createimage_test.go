//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCreateImage builds a custom AMI from the running instance and
// records the backing snapshot ID so Phase 9 cleanup can delete the
// snapshot before terminating the instance (otherwise DeleteOnTermination
// trips over the still-referenced snapshot). Maps to run-e2e.sh ~805–844.
//
// Boot-verifies both CreateImage paths: --no-reboot against the running
// singleton (IsRunning path), and a second image against the same instance
// stopped (offline path). The IsRunning path can register a well-formed AMI
// that never reaches the OS, such as one stuck at the UEFI shell on a root
// volume grown past the base image's native size — so
// describe-images reporting State=available is not itself evidence the AMI
// boots; only a real guest login is.
func runCreateImage(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — CreateImage Lifecycle")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	const customName = "e2e-custom-ami"
	const customDesc = "E2E test custom image"

	harness.Step(t, "create-image instance=%s name=%s (no-reboot)", instanceID, customName)
	customAMIID := ensureCustomAMI(t, fix, instanceID, customName, customDesc)
	require.NotEmpty(t, customAMIID, "ensureCustomAMI returned empty ImageId")
	harness.Detail(t, "custom_ami", customAMIID)

	harness.Step(t, "describe-images %s", customAMIID)
	out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(customAMIID)},
	})
	require.NoError(t, err, "describe-images %s", customAMIID)
	require.NotEmpty(t, out.Images, "no image for %s", customAMIID)
	img := out.Images[0]

	assert.Equal(t, customName, aws.StringValue(img.Name), "custom AMI Name mismatch")
	assert.Equal(t, "available", aws.StringValue(img.State), "custom AMI State should be available")

	var customAMISnapID string
	for _, bdm := range img.BlockDeviceMappings {
		if bdm.Ebs == nil {
			continue
		}
		if id := aws.StringValue(bdm.Ebs.SnapshotId); id != "" {
			customAMISnapID = id
			break
		}
	}
	if customAMISnapID == "" {
		t.Logf("WARNING: custom AMI %s has no backing snapshot ID", customAMIID)
	} else {
		harness.Detail(t, "custom_ami_snapshot", customAMISnapID)
	}

	// TODO(stage-?): bash mentions verifying the predastore-side snapshot
	// config exists for the custom AMI. The actual bash doesn't do that —
	// the EC2 API check above is the sole assertion. Wire up an S3 client
	// in the harness if we ever want to enforce the predastore-side state.

	runBootVerifyAMI(t, fix, customAMIID, "running-path")

	// The offline path: CreateImage against the same instance stopped. The
	// daemon's IsRunning decision is the instance's actual VM status at call
	// time (daemon_handlers_image.go), not the NoReboot flag, so stopping the
	// source instance is what selects snapshotStoppedVolume.
	harness.Step(t, "stop-instances %s (precondition for stopped-path CreateImage)", instanceID)
	_, err = fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "stop-instances")
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")

	// Restore the singleton to running before returning regardless of outcome
	// below — sibling Test* expect the canonical running row.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	})

	const stoppedName = "e2e-custom-ami-stopped"
	harness.Step(t, "create-image instance=%s name=%s (stopped)", instanceID, stoppedName)
	stoppedAMIID := ensureCustomAMI(t, fix, instanceID, stoppedName, "E2E test custom image (stopped path)")
	require.NotEmpty(t, stoppedAMIID, "ensureCustomAMI (stopped) returned empty ImageId")
	harness.Detail(t, "custom_ami_stopped", stoppedAMIID)

	runBootVerifyAMI(t, fix, stoppedAMIID, "stopped-path")
}

// runBootVerifyAMI launches a short-lived, test-owned instance from amiID and
// requires it to reach a real SSH login — not just EC2 State=running — so a
// well-formed-but-unbootable AMI fails this test instead of shipping
// silently. Terminates the probe instance via
// t.Cleanup regardless of outcome.
func runBootVerifyAMI(t *testing.T, fix *Fixture, amiID, label string) {
	t.Helper()
	harness.Step(t, "boot-verify %s ami=%s", label, amiID)

	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	runOut, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{ // e2e:allow-create — a dedicated boot-verify probe per produced AMI, not the shared singleton.
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: []*string{aws.String(vpc.SGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "run-instances from %s (%s)", amiID, label)
	require.NotEmpty(t, runOut.Instances, "run-instances from %s returned no Instances", amiID)
	probeInstanceID := aws.StringValue(runOut.Instances[0].InstanceId)
	require.NotEmpty(t, probeInstanceID, "run-instances from %s returned empty InstanceId", amiID)
	harness.Detail(t, "boot_verify_instance_"+label, probeInstanceID)

	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(probeInstanceID)},
		})
	})

	dir := harness.ArtifactDir(t, harness.LoadEnv(t))
	harness.OnFailure(t, func() {
		harness.DumpInstanceConsole(t, fix.AWS, probeInstanceID, dir, "createimage-"+label+"-console.log")
	})

	probeInst := harness.WaitForInstanceState(t, fix.AWS, probeInstanceID, "running")

	host, port := harness.InstancePublicSSHHost(t, probeInst)
	waitForSSHReady(t, host, port, keyPath)

	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
	idOut := runSSH(t, tgt, "id")
	assert.Containsf(t, idOut, "ubuntu", "boot-verify %s: ssh id should report ubuntu\n%s", label, idOut)

	harness.Detail(t, "boot_verify_"+label, "ok")
}
