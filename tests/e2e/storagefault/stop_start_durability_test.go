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
		t.Fatalf("guest did not come back after a same-node restart")
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

// TestCrossNodeStartAfterFailedSealIsRefused is the reproduction of the silent
// loss: a volume whose seal failed holds its only current copy on one node, and
// starting it anywhere else loads a stale checkpoint from the backend instead.
//
// Expected to fail against the current engine. The start succeeds, the guest
// comes up, and the pattern does not match — with nothing logged that names it
// as data loss.
func TestCrossNodeStartAfterFailedSealIsRefused(t *testing.T) {
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

	// Taking the daemon down on the owning node makes ec2.start.{lastNode}
	// return no responders, which is one of the three paths that silently
	// falls back to starting somewhere else.
	harness.Step(t, "stopping spinifex-daemon on %s to force the start elsewhere", hostNode.Name)
	stopDaemon(t, fix, *hostNode)

	startErr := startInstance(t, fix, instanceID)
	if startErr != nil {
		harness.Step(t, "the start was refused, which is the correct outcome: %v", startErr)
		assertRefusalNamesNode(t, startErr, *hostNode)
		return
	}

	// The start was allowed. Everything below establishes whether it was
	// harmless or whether it silently served a stale volume.
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	landed := harness.InstanceHostingNode(t, fix.Cluster, instanceID)
	if landed != nil && landed.Index == hostNode.Index {
		t.Skipf("the instance started on %s again, so no cross-node start happened and this test proved nothing", hostNode.Name)
	}

	host, port := harness.InstancePublicSSHHost(t, inst)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", tgt.KeyPath, 5*time.Minute) {
		t.Errorf("the cross-node start was allowed and the guest never came back: its root volume was "+
			"loaded from a stale backend checkpoint while %s held the current one.\n"+
			"The start should have been refused.", hostNode.Name)
		return
	}
	tgt = harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: tgt.KeyPath}

	got := readPattern(t, tgt, dev)
	if got == want {
		t.Errorf("the cross-node start was allowed. The data happened to survive this time, but %s "+
			"held the only current copy and nothing consulted it — the start must be refused, not left to chance.",
			hostNode.Name)
		return
	}
	t.Errorf("silent data loss reproduced: the instance restarted on %s while %s held the only current "+
		"copy of %s. sha256 was %s, is now %s, and the start reported success.",
		nodeName(landed), hostNode.Name, volID, want, got)
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

// assertRefusalNamesNode checks a refused start says where the data is. An
// error that only says "refused" leaves an operator with an instance they
// cannot start and no next step.
func assertRefusalNamesNode(t *testing.T, err error, node harness.Node) {
	t.Helper()
	if strings.Contains(err.Error(), node.Name) || strings.Contains(err.Error(), node.Addr) {
		harness.Step(t, "the refusal names %s, so an operator knows where the data is", node.Name)
		return
	}
	t.Errorf("the start was refused but the error does not name the node holding the data (%s/%s): %v",
		node.Name, node.Addr, err)
}

// writePattern fills the head of the device with a reproducible pattern,
// flushes it to the backend and returns its checksum. O_DIRECT plus an
// explicit flush is what makes the write acknowledged rather than merely
// buffered, which is the only kind this suite makes claims about.
func writePattern(t *testing.T, tgt harness.SSHTarget, dev string) string {
	t.Helper()
	cmd := fmt.Sprintf(
		"sudo dd if=/dev/urandom of=/dev/%s bs=1M count=%d oflag=direct status=none && "+
			"sudo blockdev --flushbufs /dev/%s && sync && "+
			"sudo dd if=/dev/%s bs=1M count=%d iflag=direct status=none | sha256sum | cut -d' ' -f1",
		dev, patternMiB, dev, dev, patternMiB)
	out, err := harness.GuestExecTimeout(tgt, cmd, 5*time.Minute)
	if err != nil {
		t.Fatalf("write pattern to /dev/%s: %v\n%s", dev, err, out)
	}
	sum := strings.TrimSpace(out)
	if len(sum) != 64 {
		t.Fatalf("expected a sha256 from the pattern write, got %q", sum)
	}
	return sum
}

// readPattern re-reads the pattern region and returns its checksum. Reads
// O_DIRECT so the guest's page cache cannot answer with what was written
// before the restart.
func readPattern(t *testing.T, tgt harness.SSHTarget, dev string) string {
	t.Helper()
	cmd := fmt.Sprintf("sudo dd if=/dev/%s bs=1M count=%d iflag=direct status=none | sha256sum | cut -d' ' -f1",
		dev, patternMiB)
	out, err := harness.GuestExecTimeout(tgt, cmd, 5*time.Minute)
	if err != nil {
		t.Fatalf("read pattern back from /dev/%s: %v\n%s", dev, err, out)
	}
	return strings.TrimSpace(out)
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

// stopDaemon stops spinifex-daemon on node and restores it on cleanup. Only
// the daemon: stopping the target would take storage down with it and change
// which fault is being tested.
func stopDaemon(t *testing.T, fix *Fixture, node harness.Node) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if _, err := fix.SSH.Run(ctx, node, "sudo systemctl start spinifex-daemon"); err != nil {
			t.Errorf("could not restart spinifex-daemon on %s — the node is left without one: %v", node.Name, err)
			return
		}
		t.Logf("restarted spinifex-daemon on %s", node.Name)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := fix.SSH.Run(ctx, node, "sudo systemctl stop spinifex-daemon"); err != nil {
		t.Fatalf("stop spinifex-daemon on %s: %v", node.Name, err)
	}
	time.Sleep(freezeSettle)
}

func errOrOK(err error) string {
	if err == nil {
		return "success"
	}
	return err.Error()
}
