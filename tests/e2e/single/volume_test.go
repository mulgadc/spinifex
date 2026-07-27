//go:build e2e

package single

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Resize parameters. The volume is created at volumeResizeFromGiB, carries a
// sentinel of volumeResizeSentinelMiB random bytes across the resize, and ends
// at volumeResizeToGiB. The 4 MiB sentinel matches the guest-churn one: large
// enough to be a meaningful checksum target, small enough that the predastore
// round-trip stays inside this phase's budget.
const (
	volumeResizeLabel       = "e2eresize"
	volumeResizeSentinelMiB = 4
	volumeResizeFromGiB     = int64(10)
	volumeResizeToGiB       = int64(20)
	bytesPerGiB             = int64(1) << 30
)

// runVolumeLifecycle exercises create → attach → write → modify → verify →
// detach → delete on a fresh 10 GiB volume against the running Phase 5
// instance, asserting the resize at both layers: the control-plane record and
// the block device the guest kernel actually sees.
//
// The resize is bracketed by a detach and a reattach because ModifyVolume
// rejects a volume whose instance is running (IncorrectState) — spinifex has no
// elastic-volume path, so the guest cannot witness a live resize and the
// reattach is what makes it observe the new geometry. The sentinel is written
// before the detach and re-read after the reattach, so the same pass proves
// both that capacity grew and that the data already on the volume survived it.
//
// Maps to run-e2e.sh ~488–612.
func runVolumeLifecycle(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Volume Lifecycle (Attach/Resize/Detach)")

	az := needAZ(t, fix)
	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	_, keyPath := needKeyPair(t, fix)

	// This phase creates + deletes a standalone test volume; do NOT route
	// through harness.EnsureVolume because the fixture's terminate-on-cleanup
	// would later try to re-delete a volume this test has just deleted in-line.
	harness.Step(t, "create-volume size=%d az=%s", volumeResizeFromGiB, az)
	// e2e:allow-create — the resized volume is the subject under test.
	createOut, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(volumeResizeFromGiB),
	})
	require.NoError(t, err, "create-volume")
	volumeID := aws.StringValue(createOut.VolumeId)
	require.NotEmpty(t, volumeID, "CreateVolume returned empty VolumeId")
	harness.Detail(t, "volume", volumeID)

	// Cleanup if a later assertion fails before the in-line delete. The volume
	// spends most of this phase attached, so a plain DeleteVolume would answer
	// VolumeInUse and strand it; RegisterVolumeTeardown force-detaches first and
	// tolerates the volume already being gone on the happy path.
	harness.RegisterVolumeTeardown(t, fix.AWS, volumeID)

	harness.WaitForVolumeState(t, fix.AWS, volumeID, "available", harness.WithPoll(500*time.Millisecond))

	tgt := resolveGuestSSHTarget(t, fix, instanceID, keyPath)

	harness.Step(t, "attach-volume %s -> %s as /dev/sdf", volumeID, instanceID)
	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volumeID, instanceID, "/dev/sdf")
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
	harness.Detail(t, "guest_dev", dev)

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

	// Pre-resize baseline. Every post-resize size assertion below is expressed
	// as growth relative to this number rather than against an absolute 20 GiB,
	// so a backend that sizes a volume a hair under the requested GiB doesn't
	// flake the test while a resize that lands short still fails it.
	preBytes := harness.LsblkDeviceBytes(t, tgt, dev)
	harness.Detail(t, "guest_bytes_before", preBytes)
	assert.InDeltaf(t, float64(volumeResizeFromGiB*bytesPerGiB), float64(preBytes), float64(bytesPerGiB),
		"guest device /dev/%s reports %d bytes, more than 1 GiB from the requested %d GiB",
		dev, preBytes, volumeResizeFromGiB)

	harness.Step(t, "write sentinel (%d MiB) to /dev/%s", volumeResizeSentinelMiB, dev)
	wantSha := harness.GuestFormatWriteSentinel(t, tgt, dev, volumeResizeLabel, volumeResizeSentinelMiB)
	harness.Detail(t, "sha256", wantSha)

	// ModifyVolume refuses an in-use volume, so detach before resizing.
	harness.Step(t, "detach-volume %s (ModifyVolume requires the volume be available)", volumeID)
	harness.DetachVolumeWait(t, fix.AWS, volumeID)

	harness.Step(t, "modify-volume size=%d", volumeResizeToGiB)
	_, err = fix.AWS.EC2.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String(volumeID),
		Size:     aws.Int64(volumeResizeToGiB),
	})
	require.NoError(t, err, "modify-volume")

	// DescribeVolumesModifications is what Terraform's aws_ebs_volume and the
	// EBS CSI driver poll to decide a resize finished, so the record has to
	// exist and reach a terminal state — a handler stuck in "modifying", or one
	// that returns nothing at all, is a resize no client can observe.
	harness.Step(t, "describe-volumes-modifications %s", volumeID)
	var mod *ec2.VolumeModification
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeVolumesModifications(&ec2.DescribeVolumesModificationsInput{
			VolumeIds: []*string{aws.String(volumeID)},
		})
		if err != nil {
			return fmt.Errorf("describe-volumes-modifications: %w", err)
		}
		if len(out.VolumesModifications) == 0 {
			return fmt.Errorf("no modification record for %s yet", volumeID)
		}
		switch state := aws.StringValue(out.VolumesModifications[0].ModificationState); state {
		case ec2.VolumeModificationStateCompleted, ec2.VolumeModificationStateOptimizing:
			mod = out.VolumesModifications[0]
			return nil
		case ec2.VolumeModificationStateFailed:
			// Terminal and unrecoverable — retrying only burns the budget.
			t.Fatalf("modification for %s reported state=failed: %s",
				volumeID, aws.StringValue(out.VolumesModifications[0].StatusMessage))
			return nil
		default:
			return fmt.Errorf("%s modification state=%q, want completed/optimizing", volumeID, state)
		}
	}, 2*time.Minute, 2*time.Second)
	assert.Equal(t, volumeID, aws.StringValue(mod.VolumeId), "modification names the wrong volume")
	assert.Equal(t, volumeResizeToGiB, aws.Int64Value(mod.TargetSize), "modification TargetSize mismatch")
	assert.Equal(t, volumeResizeFromGiB, aws.Int64Value(mod.OriginalSize), "modification OriginalSize mismatch")
	harness.Detail(t, "modification_state", aws.StringValue(mod.ModificationState),
		"target_size", aws.Int64Value(mod.TargetSize))

	// Control-plane assertion: the KV record carries the new size. This is a
	// separate claim from the guest-side one below — it proves the API answers
	// correctly, not that anything happened to the block device — and both are
	// kept because either can regress without the other.
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
		if got := aws.Int64Value(out.Volumes[0].Size); got != volumeResizeToGiB {
			return fmt.Errorf("%s size=%d want=%d", volumeID, got, volumeResizeToGiB)
		}
		return nil
	}, 5*time.Minute, 5*time.Second)
	harness.Detail(t, "resized_gib", volumeResizeToGiB)

	// Guest-side assertion: the whole point of this phase. A backend that
	// accepted ModifyVolume, updated KV, and left the underlying viperblock
	// volume alone passes everything above and fails here.
	harness.Step(t, "reattach-volume %s and assert the guest sees the new size", volumeID)
	before = harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volumeID, instanceID, "/dev/sdf")
	dev = harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
	wantBytes := preBytes + (volumeResizeToGiB-volumeResizeFromGiB)*bytesPerGiB
	postBytes := waitForGuestDeviceGrowth(t, tgt, dev, wantBytes, 60*time.Second)
	harness.Detail(t, "guest_dev", dev, "guest_bytes_after", postBytes)

	// The resize must not have disturbed what was already on the volume.
	gotSha := harness.GuestReadSentinelSha(t, tgt, "/dev/"+dev, volumeResizeLabel)
	require.Equalf(t, wantSha, gotSha, "sentinel sha256 mismatch after resize")

	// Grown capacity that no filesystem can consume is not capacity. The
	// sentinel helper formats the bare device, so there is no partition table to
	// grow first and resize2fs on its own is enough.
	harness.Step(t, "resize2fs /dev/%s and re-verify the sentinel", dev)
	fsBytes := guestGrowExt4(t, tgt, dev, volumeResizeLabel)
	// ext4 spends a few percent of the device on metadata, so the filesystem is
	// always smaller than the disk; 90% separates "grew with the device" from
	// "still sized for the old 10 GiB".
	assert.GreaterOrEqualf(t, fsBytes, postBytes*9/10,
		"ext4 on /dev/%s reports %d bytes after resize2fs; device is %d bytes, so the filesystem did not take the new capacity",
		dev, fsBytes, postBytes)
	harness.Detail(t, "fs_bytes", fsBytes)
	gotSha = harness.GuestReadSentinelSha(t, tgt, "/dev/"+dev, volumeResizeLabel)
	require.Equalf(t, wantSha, gotSha, "sentinel sha256 mismatch after resize2fs")

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

// waitForGuestDeviceGrowth polls /dev/<dev> until the guest kernel reports at
// least wantBytes, and returns the size it settled on.
//
// It deliberately does not rescan the device first: whether a resized volume
// presents its new geometry to the guest on its own is the behaviour under
// test, and a rescan up front would paper over exactly the regression this
// poll exists to catch. Only once the poll has timed out does it force a
// rescan, as a diagnostic — that turns "the resize never reached the block
// device" into "it reached the device but the guest needed a kick", which are
// different bugs with different owners. Either way the test fails.
func waitForGuestDeviceGrowth(t *testing.T, tgt harness.SSHTarget, dev string, wantBytes int64, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got int64
	for {
		got = harness.LsblkDeviceBytes(t, tgt, dev)
		if got >= wantBytes {
			return got
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if out, err := harness.GuestExec(tgt,
		fmt.Sprintf("echo 1 | sudo tee /sys/class/block/%s/device/rescan >/dev/null", dev)); err != nil {
		t.Logf("diagnostic rescan of /dev/%s failed: %v\n%s", dev, err, out)
	}
	if after := harness.LsblkDeviceBytes(t, tgt, dev); after >= wantBytes {
		t.Fatalf("guest never observed the resize on its own: /dev/%s reported %d bytes (want >= %d) for %s, "+
			"but an explicit rescan surfaced %d — the resize reaches the backend and fails to propagate to the guest",
			dev, got, wantBytes, timeout, after)
	}
	t.Fatalf("guest never observed the resize: /dev/%s reports %d bytes, want >= %d, and an explicit rescan "+
		"did not change that — the resize did not reach the block device", dev, got, wantBytes)
	return 0
}

// guestGrowExt4 grows the whole-disk ext4 on /dev/<dev> to fill the device and
// returns the mounted filesystem's total size in bytes. The resize is done
// online (mount, then resize2fs) so no offline e2fsck is needed first. The size
// is echoed behind a marker because resize2fs writes its own progress lines to
// the same stream.
func guestGrowExt4(t *testing.T, tgt harness.SSHTarget, dev, label string) int64 {
	t.Helper()
	mnt := "/mnt/" + label
	script := strings.Join([]string{
		fmt.Sprintf("sudo mkdir -p %s", mnt),
		fmt.Sprintf("sudo mount /dev/%s %s", dev, mnt),
		fmt.Sprintf("sudo resize2fs /dev/%s", dev),
		fmt.Sprintf("echo FS_BYTES=$(df -B1 --output=size %s | tail -1 | tr -d ' ')", mnt),
		fmt.Sprintf("sudo umount %s", mnt),
	}, " && ")
	out, err := harness.GuestExec(tgt, script)
	if err != nil {
		t.Fatalf("guestGrowExt4(%s): %v\n%s", dev, err, out)
	}
	m := fsBytesRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("guestGrowExt4(%s): no FS_BYTES marker in guest output:\n%s", dev, out)
	}
	size, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("guestGrowExt4(%s): parse %q: %v", dev, m[1], err)
	}
	return size
}

// fsBytesRE lifts the filesystem size out of guestGrowExt4's marker line.
var fsBytesRE = regexp.MustCompile(`FS_BYTES=(\d+)`)

// runVolumeStatus runs DescribeVolumeStatus against the root volume
// and asserts the response references it back. Maps to run-e2e.sh ~614–625.
func runVolumeStatus(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — DescribeVolumeStatus")

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
