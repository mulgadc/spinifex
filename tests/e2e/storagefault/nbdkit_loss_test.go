//go:build e2e

package storagefault

import (
	"testing"
	"time"

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
//   - SIGKILL closes the socket. QEMU's nbd driver is left at the default
//     reconnect-delay of 0, so requests fail immediately with EIO, which under
//     werror=report reaches the guest and aborts the journal.
//
// Under werror=stop,rerror=stop both must become stalls and neither may show a
// corruption signature. That is the assertion this suite exists to make.
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
			volID := harness.EnsureVolume(t, fix.Harness, az, faultVolumeSizeGiB)

			before := harness.GuestDiskSet(t, tgt)
			harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, faultDevice)
			dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)
			t.Cleanup(func() { harness.DetachVolumeWait(t, fix.AWS, volID) })

			prepareWorkloadFilesystem(t, tgt, dev)

			pid := requireNbdkitForVolume(t, fix, *hostNode, volID)
			harness.Detail(t, "instance", instanceID, "volume", volID, "guest_device", dev,
				"host_node", nodeName(hostNode), "nbdkit_pid", pid)

			harness.Step(t, "writing the %s verifiable region and flushing it", fioSize)
			writeVerifiableRegion(t, tgt)

			loadRuntime := fioSettle + nbdkitHold + recoverySettle
			harness.Step(t, "starting sustained load for %s", loadRuntime)
			startLoad(t, tgt, loadRuntime)
			time.Sleep(fioSettle)
			if !fioRunning(t, tgt) {
				t.Fatalf("fio exited before the fault was injected, so nothing was under load: %s", fioLog(t, tgt))
			}

			if tc.kill {
				harness.Step(t, "killing nbdkit for %s, then holding %s", volID, nbdkitHold)
				killNbdkit(t, fix, *hostNode, volID, pid)
			} else {
				harness.Step(t, "freezing nbdkit for %s for %s", volID, nbdkitHold)
				freezeNbdkit(t, fix, *hostNode, volID, pid)
			}

			observeDuringOutage(t, fix, instanceID, tgt)

			if !tc.kill {
				harness.Step(t, "thawing nbdkit and waiting %s", recoverySettle)
				thawNbdkit(t, fix, *hostNode, pid)
			} else {
				harness.Step(t, "nbdkit stays dead; waiting %s before judging the guest", recoverySettle)
			}
			time.Sleep(recoverySettle)

			console, consoleErr := harness.InstanceConsole(fix.AWS, instanceID)
			if consoleErr != nil {
				t.Logf("console unavailable (guest evidence only): %v", consoleErr)
			}
			evidence := console + "\n" + bestEffort(tgt, "sudo dmesg | tail -n 400")

			assertNoCorruption(t, evidence, instanceID, fix)
		})
	}
}
