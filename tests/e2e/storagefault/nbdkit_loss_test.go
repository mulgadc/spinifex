//go:build e2e

package storagefault

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// nbdkitHold is how long the volume stays unserved. Unlike the predastore
// freeze this needs no retry budget outlasting: a killed nbdkit closes its
// socket and QEMU fails the request at once, and a frozen one stalls for as
// long as it is held. Short enough that both variants fit in one run.
const nbdkitHold = 60 * time.Second

// TestGuestSurvivesNbdkitLoss removes the volume's own NBD server rather than
// the storage cluster behind it, which is a far narrower fault: one volume on
// one host, with no cluster-wide service stopped and nothing for another suite
// to trip over.
//
// The two signals are deliberately both here, because they are on opposite
// sides of the defect and running only one would prove the wrong thing:
//
//   - SIGSTOP holds the NBD socket open and answers nothing. The guest blocks.
//     No error is ever delivered, so ext4 has no reason to abort.
//   - SIGKILL closes the socket, so requests fail rather than block. Under the
//     nbd driver's default reconnect-delay of 0 they fail instantly, and under
//     werror=report that EIO reaches the guest and aborts its journal.
//
// The killed variant is put back on the same socket path afterwards, because a
// volume left dead tests the fault and not the recovery. Under
// reconnect-delay plus werror=stop,rerror=stop both must become stalls that
// resume, and neither may show a corruption signature.
func TestGuestSurvivesNbdkitLoss(t *testing.T) {
	cases := []struct {
		name string
		kill bool
	}{
		{name: "Frozen", kill: false},
		{name: "Killed", kill: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fix := requireStorageFaultFixture(t)

			instanceID, tgt := ensureFaultInstance(t, fix)
			hostNode := harness.InstanceHostingNode(t, fix.Cluster, instanceID)
			if hostNode == nil {
				t.Fatalf("cannot identify the node hosting %s, so its nbdkit cannot be found", instanceID)
			}

			az := harness.DiscoverDefaultAZ(t, fix.Harness)
			var volID string
			// quarantined is set when a replacement's exit is unconfirmed; nothing
			// that can seal the volume may then run.
			quarantined := false
			if tc.kill {
				// The replacement writes only to a volume no other test can mount.
				volID = dedicatedFaultVolume(t, fix, az, &quarantined)
			} else {
				volID = harness.EnsureVolume(t, fix.Harness, az, faultVolumeSizeGiB)
			}

			before := harness.GuestDiskSet(t, tgt)
			harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, faultDevice)
			dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)

			// rep is the nbdkit the killed variant puts back by hand. It is reaped
			// here, after the guest's quiesce cleanup, and confirmed exited
			// before the detach, so the platform's seal never runs beside it.
			var rep *replacement
			t.Cleanup(func() {
				defer func() {
					if rep != nil && t.Failed() {
						logRelaunchOutput(t, fix, *hostNode, volID)
					}
				}()
				if rep != nil {
					if !guestQuiesced(tgt) {
						t.Errorf("guest still uses %s at teardown; reaping its server anyway", faultMount)
					}
					exited := rep.reap(t)
					rep.stopGuard()
					if !exited {
						quarantined = true
						t.Errorf("left %s attached: its replacement nbdkit may still run, so no seal may run", volID)
						return
					}
				}
				harness.DetachVolumeWait(t, fix.AWS, volID)
			})

			prepareWorkloadFilesystem(t, tgt, dev)
			quiesceWorkload(t, tgt)

			pid := requireNbdkitForVolume(t, fix, *hostNode, volID)
			harness.Detail(t, "instance", instanceID, "volume", volID, "guest_device", dev,
				"host_node", nodeName(hostNode), "nbdkit_pid", pid)

			harness.Step(t, "writing the %d MiB verifiable region and flushing it", verifyMiB)
			digest := writeVerifiableRegion(t, tgt)

			loadRuntime := loadSettle + nbdkitHold + recoverySettle
			harness.Step(t, "starting sustained load for %s", loadRuntime)
			startLoad(t, tgt, loadRuntime)
			time.Sleep(loadSettle)
			if !loadRunning(t, tgt) {
				t.Fatalf("the load exited before the fault was injected, so nothing was under load: %s",
					loadLogTail(t, tgt))
			}

			var killed killedNbdkit
			if tc.kill {
				harness.Step(t, "killing nbdkit for %s, then holding %s", volID, nbdkitHold)
				killed = killNbdkit(t, fix, *hostNode, volID, pid)
			} else {
				harness.Step(t, "freezing nbdkit for %s for %s", volID, nbdkitHold)
				freezeNbdkit(t, fix, *hostNode, volID, pid)
			}

			observeDuringOutage(t, fix, instanceID, tgt)

			if tc.kill {
				harness.Step(t, "relaunching nbdkit on %s, then waiting %s", killed.socket, recoverySettle)
				rep = newReplacement(fix, *hostNode, killed.socket)
				relaunchNbdkit(t, fix, *hostNode, volID, killed, rep)
			} else {
				harness.Step(t, "thawing nbdkit and waiting %s", recoverySettle)
				thawNbdkit(t, fix, *hostNode, pid)
			}
			awaitGuestRecovered(t, tgt)

			console, consoleErr := harness.InstanceConsole(fix.AWS, instanceID)
			if consoleErr != nil {
				t.Logf("console unavailable (guest evidence only): %v", consoleErr)
			}
			evidence := console + "\n" + bestEffort(tgt, "sudo dmesg | tail -n 400")

			assertNoCorruption(t, evidence, instanceID, fix)
			assertStillWritable(t, tgt, dev)
			assertWorkloadIntegrity(t, tgt, digest)
			if tc.kill && killed.leaseEpoch != 0 {
				requireLeaseUnchanged(t, fix, volID, killed.lease)
			}
		})
	}
}

// dedicatedFaultVolume creates a volume for the killed variant alone, so a
// replacement nbdkit can never serve a volume another test mounts later.
func dedicatedFaultVolume(t *testing.T, fix *Fixture, az string, quarantined *bool) string {
	t.Helper()
	out, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(faultVolumeSizeGiB),
	})
	if err != nil {
		t.Fatalf("create-volume: %v", err)
	}
	volID := aws.StringValue(out.VolumeId)
	t.Cleanup(func() {
		if *quarantined {
			t.Errorf("kept volume %s: its replacement nbdkit's exit is unconfirmed", volID)
			return
		}
		if _, err := fix.AWS.EC2.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(volID)}); err != nil {
			t.Errorf("LEAKED volume %s: %v", volID, err)
		}
	})
	harness.WaitForVolumeState(t, fix.AWS, volID, "available", harness.WithPoll(time.Second))
	return volID
}

// guestQuiesced reports whether the load has stopped and the fault mount is
// gone; an unreachable guest is not quiesced.
func guestQuiesced(tgt harness.SSHTarget) bool {
	out := bestEffort(tgt, fmt.Sprintf(
		"p=$(sudo cat %s 2>/dev/null); if [ -n \"$p\" ] && sudo kill -0 \"$p\" 2>/dev/null; then echo BUSY; "+
			"elif mountpoint -q %s; then echo MOUNTED; else echo QUIET; fi", loadPID, faultMount))
	return strings.TrimSpace(out) == "QUIET"
}
