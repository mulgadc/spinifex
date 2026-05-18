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
	require.NotEmpty(t, fix.AZName, "Phase 1 must populate fix.AZName")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")

	harness.Step(t, "create-volume size=10 az=%s", fix.AZName)
	create, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(10),
		AvailabilityZone: aws.String(fix.AZName),
	})
	require.NoError(t, err, "create-volume")
	fix.VolumeID = aws.StringValue(create.VolumeId)
	require.NotEmpty(t, fix.VolumeID, "create-volume returned empty VolumeId")
	harness.Detail(t, "volume", fix.VolumeID)

	// Local QEMU finishes volume state transitions in <1s; the default
	// 2s polling adds avoidable wall-clock per phase. Tighten to 500ms
	// for the fast paths (create→available, attach→in-use, detach→available).
	harness.WaitForVolumeState(t, fix.AWS, fix.VolumeID, "available", harness.WithPoll(500*time.Millisecond))

	const newSize int64 = 20
	harness.Step(t, "modify-volume size=%d", newSize)
	_, err = fix.AWS.EC2.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String(fix.VolumeID),
		Size:     aws.Int64(newSize),
	})
	require.NoError(t, err, "modify-volume")

	// Bash polls Volumes[0].Size directly (not Modifications[].State) — replicate.
	// Resize is slower than state transitions; allow 5 minutes.
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(fix.VolumeID)},
		})
		if err != nil {
			return fmt.Errorf("describe-volumes: %w", err)
		}
		if len(out.Volumes) == 0 {
			return fmt.Errorf("%s not found", fix.VolumeID)
		}
		if got := aws.Int64Value(out.Volumes[0].Size); got != newSize {
			return fmt.Errorf("%s size=%d want=%d", fix.VolumeID, got, newSize)
		}
		return nil
	}, 5*time.Minute, 5*time.Second)
	harness.Detail(t, "resized_gib", newSize)

	harness.Step(t, "attach-volume %s -> %s as /dev/sdf", fix.VolumeID, fix.InstanceID)
	_, err = fix.AWS.EC2.AttachVolume(&ec2.AttachVolumeInput{
		VolumeId:   aws.String(fix.VolumeID),
		InstanceId: aws.String(fix.InstanceID),
		Device:     aws.String("/dev/sdf"),
	})
	require.NoError(t, err, "attach-volume")

	harness.WaitForVolumeState(t, fix.AWS, fix.VolumeID, ec2.VolumeStateInUse, harness.WithPoll(500*time.Millisecond))

	// Once the volume is in-use, the attachment record should be populated.
	descAttached, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(fix.VolumeID)},
	})
	require.NoError(t, err, "describe-volumes (attached)")
	require.NotEmpty(t, descAttached.Volumes[0].Attachments, "no Attachments after attach-volume")
	att := descAttached.Volumes[0].Attachments[0]
	assert.Equal(t, ec2.VolumeAttachmentStateAttached, aws.StringValue(att.State),
		"attachment State should be %q", ec2.VolumeAttachmentStateAttached)
	assert.Equal(t, fix.InstanceID, aws.StringValue(att.InstanceId), "Attachment.InstanceId mismatch")

	// Bash omits --instance-id to exercise the gateway's resolution path.
	harness.Step(t, "detach-volume %s", fix.VolumeID)
	_, err = fix.AWS.EC2.DetachVolume(&ec2.DetachVolumeInput{
		VolumeId: aws.String(fix.VolumeID),
	})
	require.NoError(t, err, "detach-volume")

	harness.WaitForVolumeState(t, fix.AWS, fix.VolumeID, "available", harness.WithPoll(500*time.Millisecond))

	harness.Step(t, "delete-volume %s", fix.VolumeID)
	_, err = fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(fix.VolumeID),
	})
	require.NoError(t, err, "delete-volume")

	// Bash treats describe-volume's first non-zero exit OR empty/None result
	// as proof of deletion. We assert on InvalidVolume.NotFound specifically —
	// any other error or a successful describe returning a non-deleted state
	// surfaces the bug instead of being papered over.
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(fix.VolumeID)},
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

	// Defensive: clear the fixture field so Stage D / G cleanup doesn't try
	// to re-delete this volume.
	fix.VolumeID = ""
}

// phase5bii_VolumeStatus runs DescribeVolumeStatus against the root volume
// and asserts the response references it back. Maps to run-e2e.sh ~614–625.
func phase5bii_VolumeStatus(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5b-ii — DescribeVolumeStatus")
	require.NotEmpty(t, fix.RootVolumeID, "Phase 5 must populate fix.RootVolumeID")

	out, err := fix.AWS.EC2.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String(fix.RootVolumeID)},
	})
	require.NoError(t, err, "describe-volume-status %s", fix.RootVolumeID)
	require.NotEmpty(t, out.VolumeStatuses, "no VolumeStatuses returned")

	var matched bool
	var status string
	for _, vs := range out.VolumeStatuses {
		if aws.StringValue(vs.VolumeId) == fix.RootVolumeID {
			matched = true
			if vs.VolumeStatus != nil {
				status = aws.StringValue(vs.VolumeStatus.Status)
			}
			break
		}
	}
	assert.Truef(t, matched, "DescribeVolumeStatus did not return %s; got %d entries",
		fix.RootVolumeID, len(out.VolumeStatuses))
	harness.Detail(t, "volume", fix.RootVolumeID, "status", status)
}
