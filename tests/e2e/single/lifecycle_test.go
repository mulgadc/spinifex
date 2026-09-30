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
// rejected with IncorrectInstanceState. Leaves the instance stopped so
// Phase 7a / 7b can act against the stopped state. Maps to run-e2e.sh
// ~1027–1053.
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

// runAttachToStoppedError creates a fresh 10 GiB volume and attempts
// to attach it to the (currently stopped) primary instance. The attach
// must fail with IncorrectInstanceState. The test volume is deleted
// in-line on success and via t.Cleanup on early failure. Maps to
// run-e2e.sh ~1054–1068.
func runAttachToStoppedError(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Attach Volume to Stopped Instance (Error Path)")

	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	az := needAZ(t, fix)

	// Stop the primary instance so the IncorrectInstanceState assertion has
	// something to bite on, then restore it at end so sibling Test* see the
	// canonical running row.
	harness.Step(t, "stop-instances %s (precondition for 7a)", instanceID)
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

	// Best-effort cleanup if the negative assertion or anything else fails
	// before we reach the explicit delete-volume below.
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		_, _ = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
			VolumeId: aws.String(stoppedVolID),
		})
	})

	harness.WaitForVolumeState(t, fix.AWS, stoppedVolID, "available")

	harness.Step(t, "attach-volume %s -> %s (should fail)", stoppedVolID, instanceID)
	harness.ExpectError(t, "IncorrectInstanceState", func() error {
		_, err := fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
			VolumeId:   aws.String(stoppedVolID),
			InstanceId: aws.String(instanceID),
			Device:     aws.String("/dev/sdg"),
		})
		return err
	})

	harness.Step(t, "delete-volume %s", stoppedVolID)
	_, err = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(stoppedVolID),
	})
	require.NoError(t, err, "delete-volume %s", stoppedVolID)
	cleaned = true
}
