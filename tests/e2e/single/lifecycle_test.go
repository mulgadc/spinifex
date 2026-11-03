//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// sshHealth is this suite's sticky SSH-datapath verdict; see harness.SSHHealth.
var sshHealth harness.SSHHealth

// runStopStart stops fix.Instance and waits for it to reach the
// "stopped" state, then asserts that rebooting a stopped instance is
// rejected with IncorrectInstanceState, then starts it again. Maps to
// run-e2e.sh ~1027–1053.
func runStopStart(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Instance State Transitions (Stop)")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	harness.Step(t, "stop-instances %s", instanceID)
	_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "stop-instances")

	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")

	// Reboot of a stopped instance must be rejected — IncorrectInstanceState
	// is the AWS code bash's expect_error pins on.
	harness.Step(t, "reboot-instances on stopped instance (should fail)")
	harness.ExpectError(t, "IncorrectInstanceState", func() error {
		_, err := fix.AWS.EC2.RebootInstances(&ec2.RebootInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		return err
	})
	harness.Detail(t, "stopped_reject", "IncorrectInstanceState")

	// Restore primary instance to running so sibling Test* don't trip on a
	// stopped singleton row.
	harness.Step(t, "start-instances %s (restore for sibling tests)", instanceID)
	_, err = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "start-instances (restore)")
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
}

// runAttachToStopped attaches a fresh 10 GiB volume to the (stopped) primary
// instance, as AWS allows: the attach edits the stopped record and claims the
// volume in-use, a delete is refused while it is attached, and a detach
// releases it to available. The volume is deleted in-line on success and via
// t.Cleanup on early failure.
func runAttachToStopped(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Attach/Detach Volume on a Stopped Instance")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	az := needAZ(t, fix)

	// Stop the primary instance, then restore it at end so sibling Test* see
	// the canonical running row.
	harness.Step(t, "stop-instances %s (precondition)", instanceID)
	_, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "stop-instances")
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	})

	harness.Step(t, "create-volume size=10 az=%s", az)
	create, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(10),
		AvailabilityZone: aws.String(az),
	})
	require.NoError(t, err, "create-volume")
	stoppedVolID := aws.StringValue(create.VolumeId)
	require.NotEmpty(t, stoppedVolID, "create-volume returned empty VolumeId")
	harness.Detail(t, "test_volume", stoppedVolID)

	// Best-effort cleanup if anything fails before the explicit delete below.
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		_, _ = fix.AWS.EC2.DetachVolume(&ec2.DetachVolumeInput{
			VolumeId:   aws.String(stoppedVolID),
			InstanceId: aws.String(instanceID),
		})
		_, _ = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
			VolumeId: aws.String(stoppedVolID),
		})
	})

	harness.WaitForVolumeState(t, fix.AWS, stoppedVolID, "available")

	harness.Step(t, "attach-volume %s -> %s (stopped)", stoppedVolID, instanceID)
	_, err = fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
		VolumeId:   aws.String(stoppedVolID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String("/dev/sdg"),
	})
	require.NoError(t, err, "attach-volume to a stopped instance")
	vol := harness.WaitForVolumeState(t, fix.AWS, stoppedVolID, "in-use")
	require.Len(t, vol.Attachments, 1, "attachments on %s", stoppedVolID)
	require.Equal(t, instanceID, aws.StringValue(vol.Attachments[0].InstanceId), "attached instance")
	require.Equal(t, "/dev/sdg", aws.StringValue(vol.Attachments[0].Device), "attached device")

	harness.Step(t, "delete-volume %s while attached (should fail)", stoppedVolID)
	harness.ExpectError(t, "VolumeInUse", func() error {
		_, err := fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
			VolumeId: aws.String(stoppedVolID),
		})
		return err
	})

	harness.Step(t, "detach-volume %s from %s (stopped)", stoppedVolID, instanceID)
	_, err = fix.AWS.EC2.DetachVolume(&ec2.DetachVolumeInput{
		VolumeId:   aws.String(stoppedVolID),
		InstanceId: aws.String(instanceID),
	})
	require.NoError(t, err, "detach-volume from a stopped instance")
	harness.WaitForVolumeState(t, fix.AWS, stoppedVolID, "available")

	harness.Step(t, "delete-volume %s", stoppedVolID)
	_, err = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(stoppedVolID),
	})
	require.NoError(t, err, "delete-volume %s", stoppedVolID)
	cleaned = true
}
