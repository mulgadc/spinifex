//go:build e2e

package storagefault

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// patternDevice is the guest-visible attach point for the pattern volume.
	patternDevice = "/dev/sdg"

	// patternVolumeSizeGiB sizes the volume holding the known pattern.
	patternVolumeSizeGiB = 1

	// patternMiB is how much of the device the pattern covers. Large enough to
	// span many 4 KiB blocks and several 4 MiB chunks, small enough to write
	// and re-read in seconds.
	patternMiB = 64

	// stopStartTimeout bounds a StopInstances or StartInstances round trip.
	// Generous because the stop is deliberately made to run against a backend
	// that cannot answer, which is the slowest this path ever gets.
	stopStartTimeout = 10 * time.Minute
)

// viperblockBaseDir is where a node keeps per-volume local state. Overridable
// for a cluster laid out differently, but this is the installed default and
// what the seal receipt sits beside.
func viperblockBaseDir() string {
	if v := os.Getenv("SPINIFEX_VIPERBLOCK_BASEDIR"); v != "" {
		return v
	}
	return "/var/lib/spinifex/viperblock"
}

// TestStopStartPreservesWritesOnSameNode proves the recovery path the whole
// dirty-volume design rests on: when a seal fails, the local WAL is kept and a
// restart on the same node recovers from it with nothing lost.
//
// Expected to pass against the current engine. It is here because the
// cross-node test below is only meaningful if this one holds — a fix that
// refused every start would make that test green while making the platform
// worse.
func TestStopStartPreservesWritesOnSameNode(t *testing.T) {
	fix := requireStorageFaultFixture(t)
	requireNoneFrozen(t, fix)

	instanceID, tgt := ensureFaultInstance(t, fix)
	hostNode := harness.InstanceHostingNode(t, fix.Cluster, instanceID)
	if hostNode == nil {
		t.Fatalf("cannot identify the node hosting %s, so its local state cannot be inspected", instanceID)
	}

	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := harness.EnsureVolume(t, fix.Harness, az, patternVolumeSizeGiB)

	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, patternDevice)
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)
	t.Cleanup(func() { harness.DetachVolumeWait(t, fix.AWS, volID) })

	want := writePattern(t, tgt, dev)
	harness.Detail(t, "instance", instanceID, "volume", volID, "host_node", hostNode.Name, "sha256", want)

	freezeSet, why := freezeSetFor(fix, hostNode)
	harness.Step(t, "freezing predastore on %s (%s) so the seal cannot complete", nodeNames(freezeSet), why)
	freezePredastore(t, fix, freezeSet)

	harness.Step(t, "stopping %s with the backend frozen", instanceID)
	stopErr := stopInstance(t, fix, instanceID)
	t.Logf("StopInstances returned: %v", errOrOK(stopErr))

	assertLocalStateKept(t, fix, hostNode, volID)

	thawPredastore(t, fix, freezeSet)
	time.Sleep(recoverySettle)

	harness.Step(t, "starting %s again on the same node", instanceID)
	startInstance(t, fix, instanceID)
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	host, port := harness.InstancePublicSSHHost(t, inst)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", tgt.KeyPath, 5*time.Minute) {
		t.Fatalf("guest did not come back after a same-node restart%s", consoleTail(fix, instanceID))
	}
	tgt = harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: tgt.KeyPath}

	got := readPattern(t, tgt, dev)
	if got != want {
		t.Errorf("a same-node restart after a failed seal lost data: sha256 was %s, now %s.\n"+
			"This breaks the premise the dirty-volume design rests on — the local WAL is supposed "+
			"to be kept and recovered from on exactly this path.", want, got)
		return
	}
	harness.Step(t, "same-node restart recovered every acknowledged write from local state")
}

// TestCrossNodeStartAfterFailedSealTakesOverLoudly pins the trade the design
// makes. A volume whose seal failed has its freshest copy on one node, and
// starting it elsewhere opens an older checkpoint from the backend.
//
// Starting elsewhere is nonetheless the right outcome: instance start forwards
// to the node that last ran the instance and only falls back once that node has
// failed its window, so refusing here would leave the instance unable to run at
// all. What must never happen is the fallback being silent, so this asserts the
// takeover is logged and names the node whose writes were left behind.
func TestCrossNodeStartAfterFailedSealTakesOverLoudly(t *testing.T) {
	fix := requireStorageFaultFixture(t)
	if len(fix.Cluster.Nodes) < 3 {
		t.Skip("a cross-node start needs somewhere else to start: single-node clusters cannot reproduce this")
	}
	requireNoneFrozen(t, fix)

	instanceID, tgt := ensureFaultInstance(t, fix)
	hostNode := harness.InstanceHostingNode(t, fix.Cluster, instanceID)
	if hostNode == nil {
		t.Fatalf("cannot identify the node hosting %s", instanceID)
	}

	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := harness.EnsureVolume(t, fix.Harness, az, patternVolumeSizeGiB)

	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, patternDevice)
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)
	t.Cleanup(func() { harness.DetachVolumeWait(t, fix.AWS, volID) })

	want := writePattern(t, tgt, dev)
	harness.Detail(t, "instance", instanceID, "volume", volID, "host_node", hostNode.Name, "sha256", want)

	freezeSet, _ := freezeSetFor(fix, hostNode)
	freezePredastore(t, fix, freezeSet)

	harness.Step(t, "stopping %s with the backend frozen, so the seal fails", instanceID)
	_ = stopInstance(t, fix, instanceID)
	assertLocalStateKept(t, fix, hostNode, volID)

	thawPredastore(t, fix, freezeSet)
	time.Sleep(recoverySettle)

	// The node has to be gone, not merely quiet. A live viperblockd goes on
	// renewing the lease on a volume whose seal failed, and that refusal is
	// correct: the node could still be writing.
	harness.Step(t, "taking %s out so the start has to happen elsewhere", hostNode.Name)
	isolateNode(t, fix, *hostNode)

	if startErr := startInstance(t, fix, instanceID); startErr != nil {
		t.Fatalf("the start was refused, so the instance can run nowhere while %s is down: %v\n"+
			"Falling back to the backend checkpoint is the intended behaviour here.", hostNode.Name, startErr)
	}

	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	landed := harness.InstanceHostingNode(t, fix.Cluster, instanceID)
	if landed != nil && landed.Index == hostNode.Index {
		t.Skipf("the instance started on %s again, so no cross-node start happened and this test proved nothing", hostNode.Name)
	}
	harness.Step(t, "the instance started on %s while %s was down", nodeName(landed), hostNode.Name)

	// The loss is accepted; being unable to see it is not. This is the only
	// record that the volume opened from an older checkpoint than existed.
	assertTakeoverLogged(t, fix, landed, hostNode, volID)

	host, port := harness.InstancePublicSSHHost(t, inst)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", tgt.KeyPath, 5*time.Minute) {
		t.Errorf("the cross-node start was allowed and the guest never came back: opening from the "+
			"backend checkpoint has to yield a bootable volume, not an unusable one.%s",
			consoleTail(fix, instanceID))
		return
	}
	tgt = harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: tgt.KeyPath}

	// Reading the pattern needs sudo, which the checkpoint may predate. Losing it
	// is the same accepted rollback, but only once cloud-init is ruled out: a guest
	// that found no datasource is the keyless-boot bug, not a storage outcome.
	if !guestSudoReady(tgt) {
		ds := guestDatasource(tgt)
		if strings.Contains(ds, "DataSourceNone") || ds == "" {
			t.Errorf("the guest came back unable to sudo and cloud-init found no datasource, so this "+
				"is IMDS failing to answer, not the checkpoint rolling back: %q%s", ds,
				consoleTail(fix, instanceID))
			return
		}
		harness.Step(t, "the pattern cannot be read: the root filesystem opened from a checkpoint "+
			"predating cloud-init's sudoers drop-in, the same documented cost as the pattern "+
			"changing. cloud-init did find its datasource (%s), so metadata was served.", ds)
		return
	}

	if got := readPattern(t, tgt, dev); got == want {
		harness.Step(t, "the pattern also survived the takeover: %s had sealed enough of it", hostNode.Name)
	} else {
		harness.Step(t, "the pattern changed across the takeover (was %s, now %s), which is the "+
			"documented cost of starting from the backend checkpoint", want, got)
	}
}

// guestSudoReady reports whether the guest can still sudo without a password.
// A volume opened from the backend checkpoint may predate cloud-init writing
// /etc/sudoers.d/90-cloud-init-users, which no read of the device can tell apart.
func guestSudoReady(tgt harness.SSHTarget) bool {
	_, err := harness.GuestExecTimeout(tgt, "sudo -n true", 60*time.Second)
	return err == nil
}

// guestDatasource returns what cloud-init recorded for this boot. result.json is
// the file that names the datasource and is world-readable; status --long is the
// fallback. This is the discriminator between a rollback and IMDS never answering.
func guestDatasource(tgt harness.SSHTarget) string {
	out, err := harness.GuestExecTimeout(tgt,
		"cat /run/cloud-init/result.json 2>/dev/null; cloud-init status --long 2>/dev/null",
		60*time.Second)
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(out), " ")
}

// assertTakeoverLogged fails unless the node that opened the volume said so.
//
// This is the whole guarantee. Losing the previous holder's unsealed writes is
// a deliberate trade against the instance not running at all, and it is only
// defensible while an operator can find out it happened.
func assertTakeoverLogged(t *testing.T, fix *Fixture, landed, previous *harness.Node, volID string) {
	t.Helper()
	if landed == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := "sudo journalctl -u spinifex-viperblock -u spinifex-daemon --since '-10 min' --no-pager 2>/dev/null | " +
		"grep -F " + volID + " | grep -i 'last held by another node' | tail -5"
	out, err := fix.SSH.Run(ctx, *landed, cmd)
	if err != nil {
		t.Errorf("could not read %s's journal to confirm the takeover was logged: %v", landed.Name, err)
		return
	}
	journal := string(out)
	if strings.TrimSpace(journal) == "" {
		t.Errorf("%s opened %s from the backend checkpoint while %s held unsealed writes, and logged "+
			"nothing about it. The loss is acceptable; a silent loss is not.",
			landed.Name, volID, previous.Name)
		return
	}
	// The marker records the spinifex node name, which is the host's own name
	// and not the harness's nodeN label, so accept either identity.
	if !strings.Contains(journal, previous.Name) && !strings.Contains(journal, previous.Addr) {
		t.Errorf("%s logged a takeover for %s but did not name %s (%s), so an operator cannot tell "+
			"whose writes were left behind:\n%s", landed.Name, volID, previous.Name, previous.Addr, journal)
		return
	}
	harness.Step(t, "%s logged the takeover and named %s", landed.Name, previous.Name)
}

// assertLocalStateKept checks the node kept the volume's local files and wrote
// no seal receipt. Both are what a correctly failed seal leaves behind, and
// together they are the ground truth a cluster-visible marker would index.
func assertLocalStateKept(t *testing.T, fix *Fixture, node *harness.Node, volID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	base := viperblockBaseDir()
	dirCmd := fmt.Sprintf("sudo test -d %s/%s && echo PRESENT || echo ABSENT", base, volID)
	dirOut, err := fix.SSH.Run(ctx, *node, dirCmd)
	if err != nil {
		t.Fatalf("inspect local volume state on %s: %v", node.Name, err)
	}
	receiptCmd := fmt.Sprintf("sudo test -f %s/%s.sealed && echo PRESENT || echo ABSENT", base, volID)
	receiptOut, err := fix.SSH.Run(ctx, *node, receiptCmd)
	if err != nil {
		t.Fatalf("inspect seal receipt on %s: %v", node.Name, err)
	}

	local := strings.TrimSpace(string(dirOut))
	receipt := strings.TrimSpace(string(receiptOut))
	harness.Detail(t, "node", node.Name, "local_state", local, "seal_receipt", receipt)

	if receipt == "PRESENT" {
		t.Errorf("a seal receipt exists for %s on %s even though the backend was frozen: "+
			"the seal reported success it cannot have had", volID, node.Name)
	}
	if local == "ABSENT" {
		t.Errorf("%s kept no local state for %s after a failed seal. The un-uploaded writes were the "+
			"only current copy and they have been deleted — this is unrecoverable data loss, not a routing bug.",
			node.Name, volID)
	}
}

// writePattern fills the head of the device with a reproducible pattern,
// flushes it to the backend and returns its checksum. O_DIRECT plus an
// explicit flush is what makes the write acknowledged rather than merely
// buffered, which is the only kind this suite makes claims about.
func writePattern(t *testing.T, tgt harness.SSHTarget, dev string) string {
	t.Helper()
	cmd := fmt.Sprintf(
		"sudo dd if=/dev/urandom of=/dev/%s bs=1M count=%d oflag=direct status=none && "+
			"sudo blockdev --flushbufs /dev/%s && sync",
		dev, patternMiB, dev)
	out, err := harness.GuestExecTimeout(tgt, cmd, 5*time.Minute)
	if err != nil {
		t.Fatalf("write pattern to /dev/%s: %v\n%s\n%s", dev, err, out, deviceGeometry(tgt, dev))
	}
	return readPattern(t, tgt, dev)
}

// readPattern re-reads the pattern region and returns its checksum. The page
// cache is dropped first so the read comes from the device rather than from
// what was written before the restart.
//
// Not O_DIRECT: this image ships uutils dd, whose O_DIRECT read path only
// handles 512-byte transfers and fails EINVAL at 4k and above. Dropping the
// cache gives the same guarantee and works under either dd.
func readPattern(t *testing.T, tgt harness.SSHTarget, dev string) string {
	t.Helper()
	cmd := fmt.Sprintf(
		"set -o pipefail; sudo blockdev --flushbufs /dev/%[1]s && sync && "+
			"echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null && "+
			"sudo dd if=/dev/%[1]s bs=1M count=%[2]d status=none | sha256sum | cut -d' ' -f1",
		dev, patternMiB)
	out, err := harness.GuestExecTimeout(tgt, cmd, 5*time.Minute)
	if err != nil {
		t.Fatalf("read pattern back from /dev/%s: %v\n%s\n%s", dev, err, out, deviceGeometry(tgt, dev))
	}
	sum := strings.TrimSpace(out)
	if len(sum) != 64 {
		t.Fatalf("expected a sha256 from /dev/%s, got %q\n%s", dev, sum, deviceGeometry(tgt, dev))
	}
	return sum
}

// stopInstance stops the instance and waits for it to settle. The error is
// returned rather than fatal: whether a stop against a dead backend succeeds
// or fails is one of the things under test.
func stopInstance(t *testing.T, fix *Fixture, instanceID string) error {
	t.Helper()
	if _, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}); err != nil {
		return err
	}
	deadline := time.Now().Add(stopStartTimeout)
	for time.Now().Before(deadline) {
		switch instanceStateOf(fix, instanceID) {
		case "stopped":
			return nil
		case "error":
			return fmt.Errorf("instance %s entered error state during stop", instanceID)
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("instance %s did not reach stopped within %s", instanceID, stopStartTimeout)
}

// startInstance starts the instance, returning the API error if the start is
// refused. A refusal is the desired behaviour for a dirty volume, so this is a
// value the caller inspects rather than a failure.
func startInstance(t *testing.T, fix *Fixture, instanceID string) error {
	t.Helper()
	_, err := fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	return err
}

// isolatedUnits are the two services that have to be down for a node to look
// gone. The daemon so ec2.start.{lastNode} has no responder, and viperblock so
// nothing on this node keeps renewing the lease it holds on the volume.
//
// Not the whole target: predastore stays up so the node keeps serving its shard,
// which is what a node whose control plane died looks like from the cluster.
var isolatedUnits = []string{"spinifex-daemon", "spinifex-viperblock"}

// leaseExpiryBudget is how long to wait for an unrenewed volume lease to age
// out of JetStream once its holder is gone. Comfortably past the 45s TTL, since
// the entry only disappears on the bucket's own schedule.
const leaseExpiryBudget = 75 * time.Second

// isolateNode stops the control-plane services on node and restores them on
// cleanup, then waits for the volume lease the node held to expire.
//
// Waiting is the point. Without it the start elsewhere is refused by a lease
// that is simply still current, which says nothing about what happens when its
// holder is not coming back.
func isolateNode(t *testing.T, fix *Fixture, node harness.Node) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		for _, unit := range isolatedUnits {
			if _, err := fix.SSH.Run(ctx, node, "sudo systemctl start "+unit); err != nil {
				t.Errorf("could not restart %s on %s — the node is left without it: %v", unit, node.Name, err)
				continue
			}
			t.Logf("restarted %s on %s", unit, node.Name)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	for _, unit := range isolatedUnits {
		if _, err := fix.SSH.Run(ctx, node, "sudo systemctl stop "+unit); err != nil {
			t.Fatalf("stop %s on %s: %v", unit, node.Name, err)
		}
	}

	harness.Step(t, "waiting %s for %s's volume lease to age out", leaseExpiryBudget, node.Name)
	time.Sleep(leaseExpiryBudget)
}

func errOrOK(err error) string {
	if err == nil {
		return "success"
	}
	return err.Error()
}

// consoleTail returns the guest's serial console for a restart that never
// answered SSH, formatted to append to a failure message. Whether the kernel
// booted and stalled or never got that far is only visible here, and the log is
// gone once the fixture terminates the instance.
func consoleTail(fix *Fixture, instanceID string) string {
	console, err := harness.InstanceConsole(fix.AWS, instanceID)
	if err != nil {
		return fmt.Sprintf("\nconsole unavailable: %v", err)
	}
	if strings.TrimSpace(console) == "" {
		return "\nconsole was empty, so the guest produced no serial output at all"
	}
	const tailBytes = 4096
	if len(console) > tailBytes {
		console = console[len(console)-tailBytes:]
	}
	return "\nguest console (last " + fmt.Sprint(len(console)) + " bytes):\n" + console
}

// deviceGeometry reports what the guest kernel thinks the device is, for a
// pattern write or read that failed. An alignment or size mismatch is the usual
// cause of an EINVAL on an O_DIRECT transfer, and neither is visible in dd's
// own message.
func deviceGeometry(tgt harness.SSHTarget, dev string) string {
	cmd := fmt.Sprintf(
		"for p in getsize64 getss getpbsz getiomin getioopt; do "+
			"printf '%%s=%%s ' $p $(sudo blockdev --$p /dev/%[1]s 2>&1); done; echo; "+
			"dd --version | head -1; "+
			"echo -n 'direct 512:  '; sudo dd if=/dev/%[1]s bs=512 count=1 iflag=direct of=/dev/null 2>&1 | tail -1; "+
			"echo -n 'direct 4k:   '; sudo dd if=/dev/%[1]s bs=4k  count=1 iflag=direct of=/dev/null 2>&1 | tail -1; "+
			"echo -n 'direct 1M:   '; sudo dd if=/dev/%[1]s bs=1M  count=1 iflag=direct of=/dev/null 2>&1 | tail -1; "+
			"echo -n 'buffered 1M: '; sudo dd if=/dev/%[1]s bs=1M  count=1 of=/dev/null 2>&1 | tail -1",
		dev)
	out, err := harness.GuestExecTimeout(tgt, cmd, time.Minute)
	if err != nil {
		return fmt.Sprintf("device geometry unavailable: %v\n%s", err, out)
	}
	return "device geometry:\n" + out
}
