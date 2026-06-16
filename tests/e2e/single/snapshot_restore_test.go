//go:build e2e

package single

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const snapshotDataLabel = "e2esnap"

// runSnapshotRestore proves snapshot→restore preserves real guest bytes. It
// drives the meaningful capture path — CreateSnapshot while the source volume
// is attached flushes a live frozen checkpoint (ebs.snapshot →
// vb.CreateSnapshot) — then restores into a new volume and reads the sentinel
// back through the guest filesystem. Sequential: mutates the singleton's
// attached devices.
func runSnapshotRestore(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Snapshot → Restore Data Fidelity")

	az := needAZ(t, fix)
	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	_, keyPath := needKeyPair(t, fix)

	host, port := harness.InstancePublicSSHHost(t, inst)
	waitForSSHReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ec2-user", Host: host, Port: port, KeyPath: keyPath}

	// 1. Data volume → attach → format → write sentinel.
	harness.Step(t, "create-volume size=1 az=%s", az)
	createOut, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(1),
	})
	require.NoError(t, err, "create-volume")
	srcVolID := aws.StringValue(createOut.VolumeId)
	require.NotEmpty(t, srcVolID, "CreateVolume returned empty VolumeId")
	harness.Detail(t, "src_volume", srcVolID)
	harness.RegisterVolumeTeardown(t, fix.AWS, srcVolID)
	harness.WaitForVolumeState(t, fix.AWS, srcVolID, "available", harness.WithPoll(500*time.Millisecond))

	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, srcVolID, instanceID, "/dev/sdf")
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
	wantSha := harness.GuestFormatWriteSentinel(t, tgt, dev, snapshotDataLabel, volumeDataSizeMiB)
	harness.Detail(t, "src_dev", dev, "sha256", wantSha)

	// 2. CreateSnapshot while attached → live checkpoint flush.
	harness.Step(t, "create-snapshot volume=%s (attached)", srcVolID)
	snapOut, err := fix.AWS.EC2.CreateSnapshot(&ec2.CreateSnapshotInput{
		VolumeId:    aws.String(srcVolID),
		Description: aws.String("e2e-snapshot-restore"),
	})
	require.NoError(t, err, "create-snapshot")
	snapID := aws.StringValue(snapOut.SnapshotId)
	require.NotEmpty(t, snapID, "CreateSnapshot returned empty SnapshotId")
	harness.Detail(t, "snapshot", snapID)
	// Registered before the restore-volume teardown so it runs last (LIFO):
	// the cloned restore volume is deleted before its base snapshot.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.DeleteSnapshot(&ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapID)})
	})
	harness.WaitForSnapshotState(t, fix.AWS, snapID, "completed", harness.WithPoll(500*time.Millisecond))

	// 3. Restore → attach → read by device; sha must match the source.
	harness.Step(t, "create-volume from snapshot=%s", snapID)
	// Size matches the 1 GiB source so the request never depends on snapshot
	// size-defaulting — that defaulting is asserted separately and would mask
	// a fidelity failure here behind an InvalidParameterValue.
	restoreOut, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		SnapshotId:       aws.String(snapID),
		Size:             aws.Int64(1),
	})
	require.NoError(t, err, "create-volume from snapshot")
	restoreVolID := aws.StringValue(restoreOut.VolumeId)
	require.NotEmpty(t, restoreVolID, "CreateVolume(SnapshotId) returned empty VolumeId")
	harness.Detail(t, "restore_volume", restoreVolID)
	harness.RegisterVolumeTeardown(t, fix.AWS, restoreVolID)
	harness.WaitForVolumeState(t, fix.AWS, restoreVolID, "available", harness.WithPoll(500*time.Millisecond))

	// Both volumes carry the same ext4 label, so mount the restore by its
	// freshly-discovered device — not by label — to avoid ambiguity.
	before = harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, restoreVolID, instanceID, "/dev/sdg")
	restoreDev := harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
	gotSha := harness.GuestReadSentinelSha(t, tgt, "/dev/"+restoreDev, snapshotDataLabel)
	require.Equalf(t, wantSha, gotSha, "sha256 mismatch between source and snapshot-restored volume")
	harness.Detail(t, "restore_dev", restoreDev, "restore_sha_ok", gotSha)
}
