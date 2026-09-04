//go:build e2e

package storagefault

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// faultDevice is the guest-visible attach point for the workload volume.
	faultDevice = "/dev/sdf"

	// faultVolumeSizeGiB sizes the scratch volume. The working set is smaller
	// still: this test is about what happens to the filesystem, not about
	// moving enough data to measure anything.
	faultVolumeSizeGiB = 2

	// faultMount is where the guest mounts the workload filesystem.
	faultMount = "/mnt/fault"

	// verifyMiB is the region written, flushed and later re-verified. The
	// verdict is taken from this and nothing else.
	verifyMiB = 256

	// loadBlocks is how many 64 KiB blocks each pass of the sustained load
	// writes: 512 MiB, to a file of its own so an interrupted load write is
	// never mistaken for lost data.
	loadBlocks = 8192

	// rootLoadDir is where the root-filesystem load runs. The user's home is on
	// the root volume; /tmp is tmpfs on this image, so a load there would move
	// no bytes on vda and the root would never see the fault at all.
	rootLoadDir = "/home/ubuntu/.storagefault-load"

	// verifyFile holds the region the verdict is taken from, and the pid and log
	// files track the two loads. All are on paths the guest always has.
	verifyFile  = faultMount + "/verify.dat"
	loadPID     = "/tmp/storagefault-load.pid"
	loadLog     = "/tmp/storagefault-load.log"
	rootLoadPID = "/tmp/storagefault-rootload.pid"
	rootLoadLog = "/tmp/storagefault-rootload.log"

	// sha256HexLen is the length of a sha256 digest in hex, used to reject a
	// truncated or error-laden reading rather than compare it.
	sha256HexLen = 64

	// rootLoadBlocks is how many 32 KiB blocks each root-load file gets, kept
	// deliberately small: the point is journal traffic on the root volume, not
	// filling a nano guest's disk.
	rootLoadBlocks = 256

	// rootDevice is the guest's root block device, whose writes are the ones
	// that decide whether the fault could reach the root filesystem.
	rootDevice = "vda"

	// rootWriteSample is the gap between the two sectors_written readings that
	// prove the root load is live. Long enough to clear a quiet moment between
	// fio's own fsyncs, short enough not to eat into the pre-fault settle.
	rootWriteSample = 3 * time.Second

	// freezeHold is how long predastore stays frozen. It is derived, not tuned:
	// viperblock tolerates maxDrainFailures=10 consecutive drain failures, and a
	// drain against a frozen backend returns at the 30s backend timeout, so the
	// earliest a write can fail is ~300s. Below that the test measures the retry
	// budget rather than the outage and passes for the wrong reason.
	freezeHold = 420 * time.Second

	// loadSettle is how long to let the load establish real in-flight I/O before
	// the fault. A freeze that lands before the first write proves nothing.
	loadSettle = 20 * time.Second

	// recoverySettle is how long to wait after the thaw for the backend to
	// answer again before judging the guest.
	recoverySettle = 30 * time.Second

	// guestRecoveryTimeout bounds the wait for a guest paused on an I/O error
	// to answer again. The resume rides the control plane's heartbeat, so this
	// covers a poll period plus the guest catching up on held requests.
	guestRecoveryTimeout = 3 * time.Minute
)

// corruptionSignatures are the kernel messages that mean the guest's
// filesystem took the fault as data loss rather than as a pause. Each is the
// guest reacting to an error it should never have been shown.
var corruptionSignatures = []*regexp.Regexp{
	regexp.MustCompile(`EXT4-fs error`),
	regexp.MustCompile(`Aborting journal`),
	regexp.MustCompile(`Remounting filesystem read-only`),
	regexp.MustCompile(`I/O error, dev \w+`),
}

// TestGuestSurvivesBackendOutage freezes predastore under a running fio
// workload and asserts the guest comes back to an intact, still-writable
// filesystem.
//
// Expected to fail against the current engine: every volume is realized with
// werror=report, so QEMU hands the guest EIO, ext4 aborts its journal and
// remounts read-only. That failure is the reproduction.
func TestGuestSurvivesBackendOutage(t *testing.T) {
	fix := requireStorageFaultFixture(t)
	requireNoneFrozen(t, fix)

	instanceID, tgt := ensureFaultInstance(t, fix)
	hostNode := harness.InstanceHostingNode(t, fix.Cluster, instanceID)
	if hostNode == nil && len(fix.Cluster.Nodes) > 1 {
		t.Fatalf("cannot identify the node hosting %s, so the freeze set cannot spare it", instanceID)
	}

	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := harness.EnsureVolume(t, fix.Harness, az, faultVolumeSizeGiB)

	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, faultDevice)
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)
	t.Cleanup(func() { harness.DetachVolumeWait(t, fix.AWS, volID) })

	prepareWorkloadFilesystem(t, tgt, dev)
	quiesceWorkload(t, tgt)

	freezeSet, why := freezeSetFor(fix, hostNode)
	harness.Detail(t, "instance", instanceID, "volume", volID, "guest_device", dev,
		"host_node", nodeName(hostNode), "freeze", why)

	harness.Step(t, "writing the %d MiB verifiable region and flushing it", verifyMiB)
	digest := writeVerifiableRegion(t, tgt)

	loadRuntime := loadSettle + freezeHold + recoverySettle
	harness.Step(t, "starting sustained load for %s, on the volume and the root filesystem", loadRuntime)
	startLoad(t, tgt, loadRuntime)
	startRootLoad(t, tgt, loadRuntime)
	time.Sleep(loadSettle)
	if !loadRunning(t, tgt) {
		t.Fatalf("the load exited before the fault was injected, so nothing was under load: %s",
			loadLogTail(t, tgt))
	}
	assertRootLoadIsWriting(t, tgt)

	harness.Step(t, "freezing predastore on %s for %s", nodeNames(freezeSet), freezeHold)
	freezePredastore(t, fix, freezeSet)

	observeDuringOutage(t, fix, instanceID, tgt)

	harness.Step(t, "thawing predastore and waiting %s for the backend to answer", recoverySettle)
	thawPredastore(t, fix, freezeSet)
	awaitGuestRecovered(t, tgt)

	// Evidence is gathered from the console as well as the guest because a
	// guest whose root went read-only may not answer SSH, and its silence is
	// the very outcome under test.
	console, consoleErr := harness.InstanceConsole(fix.AWS, instanceID)
	if consoleErr != nil {
		t.Logf("console unavailable (guest evidence only): %v", consoleErr)
	}
	guestDmesg := bestEffort(tgt, "sudo dmesg | tail -n 400")
	evidence := console + "\n" + guestDmesg

	assertNoCorruption(t, evidence, instanceID, fix)
	assertStillWritable(t, tgt, dev)
	assertWorkloadIntegrity(t, tgt, digest)
}

// prepareWorkloadFilesystem lays an ext4 filesystem on the scratch device and
// mounts it. errors=remount-ro is ext4's default and is left in place on
// purpose: it is the guest's own protection, and the test is about whether the
// guest is ever given a reason to invoke it.
func prepareWorkloadFilesystem(t *testing.T, tgt harness.SSHTarget, dev string) {
	t.Helper()
	cmd := fmt.Sprintf(
		"sudo mkfs.ext4 -q -F /dev/%s && sudo mkdir -p %s && sudo mount -o errors=remount-ro /dev/%s %s && sudo chmod 777 %s",
		dev, faultMount, dev, faultMount, faultMount)
	if out, err := harness.GuestExecTimeout(tgt, cmd, 3*time.Minute); err != nil {
		t.Fatalf("prepare workload filesystem on /dev/%s: %v\n%s", dev, err, out)
	}
}

// The workload is built from dd, sha256sum and a shell loop, all of which are
// in the base image. That is a deliberate choice over fio: this suite needs
// real in-flight I/O and a byte-exact integrity check, not a benchmark, and
// installing a package over the internet inside the guest was by some distance
// the least reliable step in the whole run.

// loadLoop keeps real I/O in flight across the fault so it lands on a busy
// device. It rewrites one file in place, which is deliberately not verified: a
// time-bounded loop is cut off mid-write by design, and judging those blocks
// would report the interruption as corruption.
func loadLoop(runtime time.Duration) string {
	return fmt.Sprintf(
		"end=$(( $(date +%%s) + %d )); "+
			"while [ \"$(date +%%s)\" -lt $end ]; do "+
			"dd if=/dev/zero of=%s/load.dat bs=64k count=%d oflag=direct conv=notrunc "+
			"status=none || exit 1; done",
		int(runtime.Seconds()), faultMount, loadBlocks)
}

// rootLoadLoop drives the root filesystem so the fault lands on vda as well as
// on the attached volume. conv=fsync forces a flush after every file, which is
// the ext4 journal commit that aborts the journal when it fails — a failed read
// of a clean page is merely retried and proves much less.
func rootLoadLoop(runtime time.Duration) string {
	return fmt.Sprintf(
		"end=$(( $(date +%%s) + %d )); i=0; "+
			"while [ \"$(date +%%s)\" -lt $end ]; do "+
			"dd if=/dev/zero of=%s/load.$((i%%4)) bs=32k count=%d conv=fsync "+
			"status=none || exit 1; i=$((i+1)); done",
		int(runtime.Seconds()), rootLoadDir, rootLoadBlocks)
}

// startRootLoad launches the root-filesystem load detached and removes its
// files afterwards, so a nano guest's 8 GiB root is not left consumed.
func startRootLoad(t *testing.T, tgt harness.SSHTarget, runtime time.Duration) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = harness.GuestExecTimeout(tgt, "sudo rm -rf "+rootLoadDir, 60*time.Second)
	})

	cmd := fmt.Sprintf("mkdir -p %s && rm -f %s; "+
		"sudo setsid bash -c 'echo $$ > %s; %s' >%s 2>&1 </dev/null &",
		rootLoadDir, rootLoadPID, rootLoadPID, rootLoadLoop(runtime), rootLoadLog)
	if out, err := harness.GuestExec(tgt, cmd); err != nil {
		t.Fatalf("start root load: %v\n%s", err, out)
	}
}

// writeVerifiableRegion lays down the region the verdict is taken from and
// returns its digest. It completes before any fault is injected, so every byte
// was acknowledged and flushed — a mismatch afterwards is lost data and never
// an interrupted write. /dev/urandom rather than zeroes so a hole or a
// short read cannot pass by looking like the data it replaced.
func writeVerifiableRegion(t *testing.T, tgt harness.SSHTarget) string {
	t.Helper()
	write := fmt.Sprintf(
		"sudo dd if=/dev/urandom of=%s bs=1M count=%d oflag=direct conv=fsync status=none && "+
			"sync && sudo sha256sum %s | cut -d' ' -f1",
		verifyFile, verifyMiB, verifyFile)
	out, err := harness.GuestExecTimeout(tgt, write, 10*time.Minute)
	if err != nil {
		t.Fatalf("could not write the verifiable region before the fault: %v\n%s", err, out)
	}
	digest := strings.TrimSpace(out)
	if len(digest) != sha256HexLen {
		t.Fatalf("expected a sha256 of the verifiable region, got %q", out)
	}
	return digest
}

// startLoad launches the sustained workload detached, so it keeps running while
// the test injects the fault. setsid survives the SSH session closing, and the
// loop records its own pid because the dd processes it spawns come and go.
func startLoad(t *testing.T, tgt harness.SSHTarget, runtime time.Duration) {
	t.Helper()
	cmd := fmt.Sprintf("rm -f %s; sudo setsid bash -c 'echo $$ > %s; %s' >%s 2>&1 </dev/null &",
		loadPID, loadPID, loadLoop(runtime), loadLog)
	if out, err := harness.GuestExec(tgt, cmd); err != nil {
		t.Fatalf("start load: %v\n%s", err, out)
	}
}

// quiesceWorkload stops the load and unmounts the fault volume before it is
// detached. This is teardown, not part of any assertion, and it matters:
// once a drive is deleted the block layer returns EIO to the guest regardless
// of werror/rerror, so hot-detaching a mounted filesystem with writes still in
// flight damages the guest whatever on-error policy is configured. A test that
// did that would be injecting a second, unrelated fault during its own
// cleanup, and the guest it left behind would fail the next subtest.
//
// Registered as a cleanup after the detach so it runs before it: t.Cleanup is
// LIFO. Every step is best effort, because a guest that is already unreachable
// is the failure under test rather than a reason to fail here.
func quiesceWorkload(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	t.Cleanup(func() {
		// Kill the loop first, then any dd it is currently inside: the other
		// order lets the loop start a replacement.
		bestEffort(tgt, fmt.Sprintf(
			"sudo kill $(cat %s) 2>/dev/null; sudo pkill -x dd 2>/dev/null; true", loadPID))
		for range 15 {
			if !loadRunning(t, tgt) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		bestEffort(tgt, fmt.Sprintf("sync; sudo umount %s 2>/dev/null; true", faultMount))
	})
}

// loadRunning reports whether the load loop is still alive. It tracks the loop
// rather than dd, which exits and restarts on every pass.
func loadRunning(t *testing.T, tgt harness.SSHTarget) bool {
	t.Helper()
	out := bestEffort(tgt, fmt.Sprintf(
		"sudo kill -0 $(cat %s 2>/dev/null) 2>/dev/null && echo RUNNING || echo GONE", loadPID))
	return strings.Contains(out, "RUNNING")
}

func loadLogTail(t *testing.T, tgt harness.SSHTarget) string {
	t.Helper()
	return bestEffort(tgt, "sudo tail -n 40 "+loadLog)
}

// observeDuringOutage records what the control plane says while the backend is
// down. These are logged rather than asserted mid-flight: the assertions that
// decide the test are made after recovery, where they are not racing a
// transient.
func observeDuringOutage(t *testing.T, fix *Fixture, instanceID string, tgt harness.SSHTarget) {
	t.Helper()
	deadline := time.Now().Add(freezeHold)
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Second)
		alive := "unreachable"
		if _, err := harness.GuestExecTimeout(tgt, "true", 10*time.Second); err == nil {
			alive = "reachable"
		}
		t.Logf("during outage: guest_ssh=%s instance_state=%s", alive, instanceStateOf(fix, instanceID))
	}
}

// assertNoCorruption is the assertion this suite exists for. A guest that was
// paused for the outage shows none of these; a guest that was handed EIO shows
// several.
func assertNoCorruption(t *testing.T, evidence, instanceID string, fix *Fixture) {
	t.Helper()
	var hits []string
	for _, re := range corruptionSignatures {
		if m := re.FindString(evidence); m != "" {
			hits = append(hits, m)
		}
	}
	if len(hits) == 0 {
		harness.Step(t, "no corruption signatures in the guest console or dmesg")
		return
	}

	harness.DumpInstanceConsole(t, fix.AWS, instanceID, fix.ArtifactDir(t), "storagefault-console")
	t.Errorf("the guest was shown I/O errors and reacted to them: %s\n"+
		"This is the bug: the backend outage reached the guest as EIO instead of pausing it, "+
		"so ext4 aborted and protected itself by going read-only. The volume is now only as "+
		"consistent as the journal managed to be.", strings.Join(hits, ", "))
}

// assertStillWritable checks the workload filesystem is mounted rw. A guest
// that survived the outage correctly needs no remount to keep working.
func assertStillWritable(t *testing.T, tgt harness.SSHTarget, dev string) {
	t.Helper()
	mounts, err := harness.GuestExecTimeout(tgt, "cat /proc/mounts", 30*time.Second)
	if err != nil {
		t.Errorf("cannot read /proc/mounts, so the guest did not come back usable: %v", err)
		return
	}
	for _, line := range strings.Split(mounts, "\n") {
		if !strings.Contains(line, faultMount) {
			continue
		}
		switch {
		case strings.Contains(line, " ro,"), strings.HasSuffix(strings.TrimSpace(line), " ro"):
			t.Errorf("%s is mounted read-only after recovery: %q", faultMount, strings.TrimSpace(line))
		default:
			harness.Step(t, "%s is still mounted read-write", faultMount)
		}
		return
	}
	t.Errorf("%s is no longer mounted at all after the outage", faultMount)
}

// assertWorkloadIntegrity re-reads the verifiable region and compares its
// digest to the one taken when it was written. This is what distinguishes "the
// guest went read-only" from "the volume lost an acknowledged write", which are
// different failures with different fixes.
//
// The cache is dropped first, so the comparison reads the volume rather than
// the page cache that has been holding the same bytes since before the fault.
func assertWorkloadIntegrity(t *testing.T, tgt harness.SSHTarget, want string) {
	t.Helper()
	check := fmt.Sprintf(
		"sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null; "+
			"sudo sha256sum %s | cut -d' ' -f1", verifyFile)
	out, err := harness.GuestExecTimeout(tgt, check, 10*time.Minute)
	if err != nil {
		t.Errorf("could not re-read the verifiable region after recovery: %v\n%s", err, out)
		return
	}
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("the verifiable region changed across the outage — an acknowledged write did not survive:\n"+
			"  wrote  %s\n  read   %s", want, got)
		return
	}
	harness.Step(t, "sha256 verification passed: every acknowledged write survived")
}

// bestEffort runs a guest command and returns its output, or a marker when the
// guest cannot answer. Used for evidence gathering, where an unreachable guest
// is itself information and must not abort the run.
func bestEffort(tgt harness.SSHTarget, cmd string) string {
	out, err := harness.GuestExecTimeout(tgt, cmd, 30*time.Second)
	if err != nil {
		return fmt.Sprintf("<guest unreachable: %v>", err)
	}
	return out
}

// awaitGuestRecovered waits for a guest that the fault paused to answer again.
// It never fails the test: whether the guest came back is what the assertions
// after it are for, and they say far more about why than a timeout here would.
func awaitGuestRecovered(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	time.Sleep(recoverySettle)
	if harness.TryGuestSSHReady(tgt.Host, tgt.Port, tgt.User, tgt.KeyPath, guestRecoveryTimeout) {
		return
	}
	harness.Step(t, "guest still not answering %s after the backend came back", tgt.Host)
}

// lastLines keeps the tail of a console log. The whole buffer is 64 KiB of
// boot messages, and what matters is what the guest said most recently.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// instanceStateOf reports the EC2 state, or why it could not be read. It never
// fails the test: the control plane being unreachable during the outage is an
// observation, and the assertions that decide the run are made after recovery.
func instanceStateOf(fix *Fixture, instanceID string) string {
	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	if err != nil {
		return fmt.Sprintf("<describe failed: %v>", err)
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.State != nil && inst.State.Name != nil {
				return *inst.State.Name
			}
		}
	}
	return "<unknown>"
}

func nodeName(n *harness.Node) string {
	if n == nil {
		return "<unidentified>"
	}
	return n.Name
}

func nodeNames(nodes []harness.Node) string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return strings.Join(out, ", ")
}

// ensureFaultInstance launches the guest this suite drives. The smallest
// catalog entry is enough: the workload is bounded by the volume, not by the
// guest's CPU or memory.
//
// Each test gets its own guest. Every fault here is meant to hurt, and a guest
// that survived one is not a clean starting point for the next: a wedged jbd2
// or a filesystem the kernel has given up on outlives the test that caused it,
// and the next test then fails on damage it did not do.
func ensureFaultInstance(t *testing.T, fix *Fixture) (instanceID string, tgt harness.SSHTarget) {
	t.Helper()
	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	ami := harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	instanceID = harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     vpc.SubnetID,
		SGID:         vpc.SGID,
		Scope:        t.Name(),
	})
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	host, port := harness.InstancePublicSSHHost(t, inst)

	harness.Step(t, "waiting for guest SSH on %s:%d (instance_type=%s)", host, port, instType)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", keyPath, 5*time.Minute) {
		// The instance is shared across this package's tests, so an unreachable
		// guest is usually something an earlier test left behind. The console
		// is the only evidence, and terminating it takes that away.
		console, err := harness.InstanceConsole(fix.AWS, instanceID)
		if err != nil {
			console = fmt.Sprintf("console unavailable: %v", err)
		}
		t.Fatalf("guest %s SSH %s:%d not ready after 5m\nconsole tail:\n%s",
			instanceID, host, port, lastLines(console, 60))
	}
	return instanceID, harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
}

// rootSectorsWritten reads sectors_written for the root device. In
// /proc/diskstats the device name is the third field and sectors_written the
// tenth, counted from the start of the line rather than from the name.
func rootSectorsWritten(tgt harness.SSHTarget) (int64, error) {
	out, err := harness.GuestExecTimeout(tgt,
		"awk '$3==\""+rootDevice+"\"{print $10}' /proc/diskstats", 30*time.Second)
	if err != nil {
		return 0, fmt.Errorf("read /proc/diskstats: %w (%s)", err, out)
	}
	field := strings.TrimSpace(out)
	if field == "" {
		return 0, fmt.Errorf("no %s row in /proc/diskstats:\n%s", rootDevice,
			bestEffort(tgt, "cat /proc/diskstats"))
	}
	n, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sectors_written %q: %w", field, err)
	}
	return n, nil
}

// assertRootLoadIsWriting confirms the root load is both running and actually
// moving bytes on the root device. A load that produced no writes would make a
// clean run mean nothing, since the fault could never have reached the root
// filesystem at all. Two samples, because a non-zero total is only the boot's
// own writes; an increase is what proves the load is live right now.
func assertRootLoadIsWriting(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	out, err := harness.GuestExecTimeout(tgt, "pgrep -f 'name=rootload' >/dev/null && echo RUNNING", 30*time.Second)
	if err != nil || !strings.Contains(out, "RUNNING") {
		t.Fatalf("the root-filesystem load is not running, so %s is idle and the fault cannot reach it:\n%s",
			rootDevice, bestEffort(tgt, "tail -n 30 ~/rootfio.log"))
	}

	before, err := rootSectorsWritten(tgt)
	if err != nil {
		t.Fatalf("cannot confirm the root load is writing: %v", err)
	}
	time.Sleep(rootWriteSample)
	after, err := rootSectorsWritten(tgt)
	if err != nil {
		t.Fatalf("cannot confirm the root load is writing: %v", err)
	}
	if after <= before {
		t.Fatalf("%s took no writes over %s (sectors_written stayed at %d), so a clean run would prove nothing:\n%s",
			rootDevice, rootWriteSample, after, bestEffort(tgt, "tail -n 30 ~/rootfio.log"))
	}
	harness.Step(t, "root load is writing: %s gained %d sectors (%d KiB) in %s",
		rootDevice, after-before, (after-before)/2, rootWriteSample)
}
