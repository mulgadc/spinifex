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
	require.NotEmpty(t, fix.RootVolumeID, "Phase 5 must populate fix.RootVolumeID")

	// Pin the API-reported root volume size so we can cross-check the
	// snapshot's VolumeSize field.
	descVol, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(fix.RootVolumeID)},
	})
	require.NoError(t, err, "describe-volumes %s", fix.RootVolumeID)
	require.NotEmpty(t, descVol.Volumes, "no volume for %s", fix.RootVolumeID)
	rootSize := aws.Int64Value(descVol.Volumes[0].Size)

	harness.Step(t, "create-snapshot volume=%s", fix.RootVolumeID)
	const origDesc = "e2e-test-snapshot"
	snap, err := fix.AWS.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:    aws.String(fix.RootVolumeID),
		Description: aws.String(origDesc),
	})
	require.NoError(t, err, "create-snapshot")
	fix.SnapshotID = aws.StringValue(snap.SnapshotId)
	require.NotEmpty(t, fix.SnapshotID, "create-snapshot returned empty SnapshotId")

	// Verify the create response itself — bash treats these as load-bearing
	// because they prove the response shape matches AWS before any polling.
	assert.Equal(t, fix.RootVolumeID, aws.StringValue(snap.VolumeId), "snapshot VolumeId mismatch")
	assert.Equal(t, rootSize, aws.Int64Value(snap.VolumeSize), "snapshot VolumeSize mismatch")
	assert.NotEmpty(t, aws.StringValue(snap.State), "snapshot State should be populated")
	assert.NotEmpty(t, aws.StringValue(snap.Progress), "snapshot Progress should be populated")
	harness.Detail(t,
		"snapshot", fix.SnapshotID,
		"size", rootSize,
		"state", aws.StringValue(snap.State),
		"progress", aws.StringValue(snap.Progress),
	)

	harness.WaitForSnapshotState(t, fix.AWS, fix.SnapshotID, "completed")

	harness.Step(t, "describe-snapshots %s", fix.SnapshotID)
	desc, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{aws.String(fix.SnapshotID)},
	})
	require.NoError(t, err, "describe-snapshots %s", fix.SnapshotID)
	require.NotEmpty(t, desc.Snapshots, "describe-snapshots returned nothing")
	got := desc.Snapshots[0]
	assert.Equal(t, fix.RootVolumeID, aws.StringValue(got.VolumeId), "describe VolumeId mismatch")
	assert.Equal(t, rootSize, aws.Int64Value(got.VolumeSize), "describe VolumeSize mismatch")
	assert.Equal(t, origDesc, aws.StringValue(got.Description), "describe Description mismatch")

	// CopySnapshot expects the source region — pulled off the configured
	// EC2 client so the test honours SPINIFEX_AWS_REGION overrides.
	region := aws.StringValue(fix.AWS.EC2.Config.Region)
	const copyDesc = "e2e-copy"
	harness.Step(t, "copy-snapshot src=%s region=%s", fix.SnapshotID, region)
	copyOut, err := fix.AWS.EC2.CopySnapshot(&ec2.CopySnapshotInput{
		SourceSnapshotId: aws.String(fix.SnapshotID),
		SourceRegion:     aws.String(region),
		Description:      aws.String(copyDesc),
	})
	require.NoError(t, err, "copy-snapshot")
	fix.CopySnapshotID = aws.StringValue(copyOut.SnapshotId)
	require.NotEmpty(t, fix.CopySnapshotID, "copy-snapshot returned empty SnapshotId")
	require.NotEqual(t, fix.SnapshotID, fix.CopySnapshotID, "copy snapshot ID should differ from original")
	harness.Detail(t, "copy_snapshot", fix.CopySnapshotID)

	harness.WaitForSnapshotState(t, fix.AWS, fix.CopySnapshotID, "completed")

	harness.Step(t, "describe-snapshots %s,%s (both visible)", fix.SnapshotID, fix.CopySnapshotID)
	both, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{aws.String(fix.SnapshotID), aws.String(fix.CopySnapshotID)},
	})
	require.NoError(t, err, "describe-snapshots both")
	require.Lenf(t, both.Snapshots, 2, "expected 2 snapshots, got %d", len(both.Snapshots))

	// Verify the copy carries the new description.
	var copyDescGot string
	for _, s := range both.Snapshots {
		if aws.StringValue(s.SnapshotId) == fix.CopySnapshotID {
			copyDescGot = aws.StringValue(s.Description)
			break
		}
	}
	assert.Equal(t, copyDesc, copyDescGot, "copy Description mismatch")

	harness.Step(t, "delete-snapshot %s (original)", fix.SnapshotID)
	_, err = fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(fix.SnapshotID),
	})
	require.NoError(t, err, "delete-snapshot original")

	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
			SnapshotIds: []*string{aws.String(fix.SnapshotID)},
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
			fix.SnapshotID, aws.StringValue(out.Snapshots[0].State))
	}, 2*time.Minute, 2*time.Second)

	// Original is gone, so consume the fixture slot — Stage G cleanup
	// shouldn't try to re-delete.
	deletedOriginal := fix.SnapshotID
	fix.SnapshotID = ""

	harness.Step(t, "verify copy %s still completed", fix.CopySnapshotID)
	copyDescOut, err := fix.AWS.EC2.DescribeSnapshots(&ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{aws.String(fix.CopySnapshotID)},
	})
	require.NoError(t, err, "describe-snapshots copy")
	require.NotEmpty(t, copyDescOut.Snapshots, "copy disappeared after original delete")
	assert.Equal(t, "completed", aws.StringValue(copyDescOut.Snapshots[0].State),
		"copy snapshot should remain completed after original delete")

	harness.Step(t, "delete-snapshot %s (copy)", fix.CopySnapshotID)
	_, err = fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(fix.CopySnapshotID),
	})
	require.NoError(t, err, "delete-snapshot copy")
	fix.CopySnapshotID = ""

	harness.Detail(t, "deleted_original", deletedOriginal)
}

// phase5d_SnapshotBackedLaunch verifies the AMI used for the live instance
// carries a snapshot reference — proof the launch went through the
// cloneAMIToVolume → OpenFromSnapshot path. Maps to run-e2e.sh ~788–803.
//
// Bash's prose mentions verifying the predastore-side snapshot config, but
// the actual bash only checks the EC2 API. We follow the bash.
func phase5d_SnapshotBackedLaunch(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5d — Snapshot-Backed Instance Launch")
	require.NotEmpty(t, fix.AMIID, "Phase 4 must populate fix.AMIID")

	out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(fix.AMIID)},
	})
	require.NoError(t, err, "describe-images %s", fix.AMIID)
	require.NotEmpty(t, out.Images, "no image for %s", fix.AMIID)

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
		"AMI %s has no BlockDeviceMappings[].Ebs.SnapshotId — launch was NOT snapshot-backed", fix.AMIID)
	harness.Detail(t, "ami", fix.AMIID, "snapshot", snapID)

	// TODO(stage-?): bash mentions verifying the predastore-side
	// `snap-{amiId}/config.json` exists with SnapshotID + SourceVolumeName
	// populated. The current bash doesn't actually do that — if we ever
	// want to enforce it we need an S3 client wired through the harness.
}
