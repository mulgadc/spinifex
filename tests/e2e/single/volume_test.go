//go:build e2e

package single

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// phase5b_VolumeLifecycle exercises create → modify → attach → detach →
// delete on a fresh 10 GiB volume against the running Phase 5 instance.
// Maps to run-e2e.sh ~488–612.
func phase5b_VolumeLifecycle(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5b — Volume Lifecycle (Attach/Detach)")

	az := needAZ(t, fix)
	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)

	// This phase creates + deletes a standalone test volume; do NOT route
	// through harness.EnsureVolume because the fixture's terminate-on-cleanup
	// would later try to re-delete a volume this test has just deleted in-line.
	harness.Step(t, "create-volume size=10 az=%s", az)
	createOut, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(10),
	})
	require.NoError(t, err, "create-volume")
	volumeID := aws.StringValue(createOut.VolumeId)
	require.NotEmpty(t, volumeID, "CreateVolume returned empty VolumeId")
	harness.Detail(t, "volume", volumeID)

	// Best-effort cleanup if a later assertion fails before the in-line delete.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)})
	})

	harness.WaitForVolumeState(t, fix.AWS, volumeID, "available", harness.WithPoll(500*time.Millisecond))

	const newSize int64 = 20
	harness.Step(t, "modify-volume size=%d", newSize)
	_, err = fix.AWS.EC2.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String(volumeID),
		Size:     aws.Int64(newSize),
	})
	require.NoError(t, err, "modify-volume")

	// Bash polls Volumes[0].Size directly (not Modifications[].State) — replicate.
	// Resize is slower than state transitions; allow 5 minutes.
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(volumeID)},
		})
		if err != nil {
			return fmt.Errorf("describe-volumes: %w", err)
		}
		if len(out.Volumes) == 0 {
			return fmt.Errorf("%s not found", volumeID)
		}
		if got := aws.Int64Value(out.Volumes[0].Size); got != newSize {
			return fmt.Errorf("%s size=%d want=%d", volumeID, got, newSize)
		}
		return nil
	}, 5*time.Minute, 5*time.Second)
	harness.Detail(t, "resized_gib", newSize)

	harness.Step(t, "attach-volume %s -> %s as /dev/sdf", volumeID, instanceID)
	_, err = fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String("/dev/sdf"),
	})
	require.NoError(t, err, "attach-volume")

	harness.WaitForVolumeState(t, fix.AWS, volumeID, ec2.VolumeStateInUse, harness.WithPoll(500*time.Millisecond))

	// Once the volume is in-use, the attachment record should be populated.
	descAttached, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(volumeID)},
	})
	require.NoError(t, err, "describe-volumes (attached)")
	require.NotEmpty(t, descAttached.Volumes[0].Attachments, "no Attachments after attach-volume")
	att := descAttached.Volumes[0].Attachments[0]
	assert.Equal(t, ec2.VolumeAttachmentStateAttached, aws.StringValue(att.State),
		"attachment State should be %q", ec2.VolumeAttachmentStateAttached)
	assert.Equal(t, instanceID, aws.StringValue(att.InstanceId), "Attachment.InstanceId mismatch")

	// Bash omits --instance-id to exercise the gateway's resolution path.
	harness.Step(t, "detach-volume %s", volumeID)
	_, err = fix.AWS.EC2.DetachVolume(&ec2.DetachVolumeInput{
		VolumeId: aws.String(volumeID),
	})
	require.NoError(t, err, "detach-volume")

	harness.WaitForVolumeState(t, fix.AWS, volumeID, "available", harness.WithPoll(500*time.Millisecond))

	harness.Step(t, "delete-volume %s", volumeID)
	_, err = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	})
	require.NoError(t, err, "delete-volume")

	// Bash treats describe-volume's first non-zero exit OR empty/None result
	// as proof of deletion. We assert on InvalidVolume.NotFound specifically —
	// any other error or a successful describe returning a non-deleted state
	// surfaces the bug instead of being papered over.
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(volumeID)},
		})
		if err != nil {
			if harness.ErrorCodeIs(err, "InvalidVolume.NotFound") {
				return nil
			}
			return fmt.Errorf("describe-volumes: %w", err)
		}
		if len(out.Volumes) == 0 {
			return nil
		}
		if state := aws.StringValue(out.Volumes[0].State); state == "deleted" {
			return nil
		} else {
			return errors.New("volume still present: state=" + state)
		}
	}, 2*time.Minute, 2*time.Second)
}

// phase5bii_VolumeStatus runs DescribeVolumeStatus against the root volume
// and asserts the response references it back. Maps to run-e2e.sh ~614–625.
func phase5bii_VolumeStatus(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5b-ii — DescribeVolumeStatus")

	_, rootVolumeID := needInstance(t, fix)

	out, err := fix.AWS.EC2.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String(rootVolumeID)},
	})
	require.NoError(t, err, "describe-volume-status %s", rootVolumeID)
	require.NotEmpty(t, out.VolumeStatuses, "no VolumeStatuses returned")

	var matched bool
	var status string
	for _, vs := range out.VolumeStatuses {
		if aws.StringValue(vs.VolumeId) == rootVolumeID {
			matched = true
			if vs.VolumeStatus != nil {
				status = aws.StringValue(vs.VolumeStatus.Status)
			}
			break
		}
	}
	assert.Truef(t, matched, "DescribeVolumeStatus did not return %s; got %d entries",
		rootVolumeID, len(out.VolumeStatuses))
	harness.Detail(t, "volume", rootVolumeID, "status", status)
}
