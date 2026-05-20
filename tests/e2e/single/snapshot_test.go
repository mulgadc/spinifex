//go:build e2e

package single

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// phase5c_SnapshotLifecycle creates, describes, copies, and deletes a
// snapshot off the running instance's root volume (the only volume that's
// definitely backed by a mounted viperblock instance — create-snapshot
// requires that). Maps to run-e2e.sh ~627–786.
func phase5c_SnapshotLifecycle(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5c — Snapshot Lifecycle")

	_, rootVolumeID := needInstance(t, fix)

	// Pin the API-reported root volume size so we can cross-check the
	// snapshot's VolumeSize field.
	descVol, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(rootVolumeID)},
	})
	require.NoError(t, err, "describe-volumes %s", rootVolumeID)
	require.NotEmpty(t, descVol.Volumes, "no volume for %s", rootVolumeID)
	rootSize := aws.Int64Value(descVol.Volumes[0].Size)

	// Local CreateSnapshot — this phase deletes the snapshot in-line, so do
	// NOT route through harness.EnsureSnapshot (its fixture cleanup would
	// later try to re-delete).
	harness.Step(t, "create-snapshot volume=%s", rootVolumeID)
	const origDesc = "e2e-test-snapshot"
	createOut, err := fix.AWS.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:    aws.String(rootVolumeID),
		Description: aws.String(origDesc),
	})
	require.NoError(t, err, "create-snapshot")
	snapshotID := aws.StringValue(createOut.SnapshotId)
	require.NotEmpty(t, snapshotID, "CreateSnapshot returned empty SnapshotId")
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshotID)})
	})
	harness.WaitForSnapshotState(t, fix.AWS, snapshotID, "completed", harness.WithPoll(500*time.Millisecond))
	harness.Detail(t, "snapshot", snapshotID, "size", rootSize)

	harness.Step(t, "describe-snapshots %s", snapshotID)
	desc, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{aws.String(snapshotID)},
	})
	require.NoError(t, err, "describe-snapshots %s", snapshotID)
	require.NotEmpty(t, desc.Snapshots, "describe-snapshots returned nothing")
	got := desc.Snapshots[0]
	assert.Equal(t, rootVolumeID, aws.StringValue(got.VolumeId), "describe VolumeId mismatch")
	assert.Equal(t, rootSize, aws.Int64Value(got.VolumeSize), "describe VolumeSize mismatch")
	assert.Equal(t, origDesc, aws.StringValue(got.Description), "describe Description mismatch")

	// CopySnapshot expects the source region — pulled off the configured
	// EC2 client so the test honours SPINIFEX_AWS_REGION overrides.
	region := aws.StringValue(fix.AWS.EC2.Config.Region)
	const copyDesc = "e2e-copy"
	harness.Step(t, "copy-snapshot src=%s region=%s", snapshotID, region)
	copyOut, err := fix.AWS.EC2.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: aws.String(snapshotID),
		SourceRegion:     aws.String(region),
		Description:      aws.String(copyDesc),
	})
	require.NoError(t, err, "copy-snapshot")
	copySnapshotID := aws.StringValue(copyOut.SnapshotId)
	require.NotEmpty(t, copySnapshotID, "copy-snapshot returned empty SnapshotId")
	require.NotEqual(t, snapshotID, copySnapshotID, "copy snapshot ID should differ from original")
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{SnapshotId: aws.String(copySnapshotID)})
	})
	harness.Detail(t, "copy_snapshot", copySnapshotID)

	harness.WaitForSnapshotState(t, fix.AWS, copySnapshotID, "completed", harness.WithPoll(500*time.Millisecond))

	harness.Step(t, "describe-snapshots %s,%s (both visible)", snapshotID, copySnapshotID)
	both, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{aws.String(snapshotID), aws.String(copySnapshotID)},
	})
	require.NoError(t, err, "describe-snapshots both")
	require.Lenf(t, both.Snapshots, 2, "expected 2 snapshots, got %d", len(both.Snapshots))

	// Verify the copy carries the new description.
	var copyDescGot string
	for _, s := range both.Snapshots {
		if aws.StringValue(s.SnapshotId) == copySnapshotID {
			copyDescGot = aws.StringValue(s.Description)
			break
		}
	}
	assert.Equal(t, copyDesc, copyDescGot, "copy Description mismatch")

	harness.Step(t, "delete-snapshot %s (original)", snapshotID)
	_, err = fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(snapshotID),
	})
	require.NoError(t, err, "delete-snapshot original")

	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
			SnapshotIds: []*string{aws.String(snapshotID)},
		})
		if err != nil {
			if harness.ErrorCodeIs(err, "InvalidSnapshot.NotFound") {
				return nil
			}
			return fmt.Errorf("describe-snapshots: %w", err)
		}
		if len(out.Snapshots) == 0 {
			return nil
		}
		return fmt.Errorf("snapshot %s still visible (state=%s)",
			snapshotID, aws.StringValue(out.Snapshots[0].State))
	}, 2*time.Minute, 2*time.Second)

	harness.Step(t, "verify copy %s still completed", copySnapshotID)
	copyDescOut, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{aws.String(copySnapshotID)},
	})
	require.NoError(t, err, "describe-snapshots copy")
	require.NotEmpty(t, copyDescOut.Snapshots, "copy disappeared after original delete")
	assert.Equal(t, "completed", aws.StringValue(copyDescOut.Snapshots[0].State),
		"copy snapshot should remain completed after original delete")

	harness.Step(t, "delete-snapshot %s (copy)", copySnapshotID)
	_, err = fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(copySnapshotID),
	})
	require.NoError(t, err, "delete-snapshot copy")

	harness.Detail(t, "deleted_original", snapshotID)
}

// phase5d_SnapshotBackedLaunch verifies the AMI used for the live instance
// carries a snapshot reference — proof the launch went through the
// cloneAMIToVolume → OpenFromSnapshot path. Maps to run-e2e.sh ~788–803.
//
// Bash's prose mentions verifying the predastore-side snapshot config, but
// the actual bash only checks the EC2 API. We follow the bash.
func phase5d_SnapshotBackedLaunch(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5d — Snapshot-Backed Instance Launch")

	amiID := needAMI(t, fix)

	out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(amiID)},
	})
	require.NoError(t, err, "describe-images %s", amiID)
	require.NotEmpty(t, out.Images, "no image for %s", amiID)

	var snapID string
	for _, bdm := range out.Images[0].BlockDeviceMappings {
		if bdm.Ebs == nil {
			continue
		}
		if id := aws.StringValue(bdm.Ebs.SnapshotId); id != "" {
			snapID = id
			break
		}
	}
	require.NotEmptyf(t, snapID,
		"AMI %s has no BlockDeviceMappings[].Ebs.SnapshotId — launch was NOT snapshot-backed", amiID)
	harness.Detail(t, "ami", amiID, "snapshot", snapID)

	// TODO(stage-?): bash mentions verifying the predastore-side
	// `snap-{amiId}/config.json` exists with SnapshotID + SourceVolumeName
	// populated. The current bash doesn't actually do that — if we ever
	// want to enforce it we need an S3 client wired through the harness.
}
