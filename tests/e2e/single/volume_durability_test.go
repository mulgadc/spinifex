//go:build e2e

package single

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// volumeDataLabel / volumeDataSizeMiB parameterise the durability round-trip.
// A 1 GiB volume is the AWS minimum; the 4 MiB random payload is large enough
// to be a meaningful checksum target while keeping the predastore round-trip
// fast.
const (
	volumeDataLabel   = "e2edur"
	volumeDataSizeMiB = 4
)

// runVolumeDurability proves real guest bytes survive the full
// QEMU↔viperblock↔predastore path across detach/reattach and an instance
// stop/start — the assembled I/O path that no unit test reaches. It also
// asserts CreateVolume reports Encrypted==true, confirming the
// control-plane→viperblockd master-key wiring is live (on-by-default cluster).
//
// Sequential: it stops/starts the shared singleton, leaving it running so
// sibling tests are unaffected (same contract as runStopStart).
func runVolumeDurability(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Volume Data Durability Round-Trip")

	az := needAZ(t, fix)
	inst, _ := needInstance(t, fix)
	instanceID := aws.StringValue(inst.InstanceId)
	_, keyPath := needKeyPair(t, fix)

	host, port := harness.InstancePublicSSHHost(t, inst)
	waitForSSHReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	// 1. Create volume + assert the encryption-wiring signal.
	harness.Step(t, "create-volume size=1 az=%s", az)
	createOut, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(1),
	})
	require.NoError(t, err, "create-volume")
	volID := aws.StringValue(createOut.VolumeId)
	require.NotEmpty(t, volID, "CreateVolume returned empty VolumeId")
	harness.Detail(t, "volume", volID, "encrypted", aws.BoolValue(createOut.Encrypted))
	assert.Truef(t, aws.BoolValue(createOut.Encrypted),
		"CreateVolume.Encrypted should be true (on-by-default cluster master key); "+
			"false means the control-plane→viperblockd key wiring is broken")
	harness.RegisterVolumeTeardown(t, fix.AWS, volID)
	harness.WaitForVolumeState(t, fix.AWS, volID, "available", harness.WithPoll(500*time.Millisecond))

	// 2. Attach → format → write sentinel → record sha256.
	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, "/dev/sdf")
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
	harness.Detail(t, "guest_dev", dev)
	wantSha := harness.GuestFormatWriteSentinel(t, tgt, dev, volumeDataLabel, volumeDataSizeMiB)
	harness.Detail(t, "sha256", wantSha)

	// 3. Detach → reattach → re-read by device; sha must survive.
	harness.DetachVolumeWait(t, fix.AWS, volID)
	before = harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, "/dev/sdf")
	dev = harness.WaitForNewGuestDisk(t, tgt, before, 60*time.Second)
	gotSha := harness.GuestReadSentinelSha(t, tgt, "/dev/"+dev, volumeDataLabel)
	require.Equalf(t, wantSha, gotSha, "sha256 mismatch after detach/reattach")
	harness.Detail(t, "reattach_sha_ok", gotSha)

	// 4. Stop/start (forces re-mount after instance restart) → re-read by
	//    label, since the device name can shift across the restart.
	harness.Step(t, "stop-instances %s", instanceID)
	_, err = fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "stop-instances")
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "stopped")

	harness.Step(t, "start-instances %s", instanceID)
	_, err = fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "start-instances")
	runInst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")

	// SSH endpoint may have rebound after the restart — re-discover.
	host, port = harness.InstancePublicSSHHost(t, runInst)
	waitForSSHReady(t, host, port, keyPath)
	tgt = harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	gotSha = harness.GuestReadSentinelSha(t, tgt, "/dev/disk/by-label/"+volumeDataLabel, volumeDataLabel)
	require.Equalf(t, wantSha, gotSha, "sha256 mismatch after stop/start")
	harness.Detail(t, "stopstart_sha_ok", gotSha)
}
