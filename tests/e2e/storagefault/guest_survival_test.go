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

	// fioSize is the region written, flushed and later re-verified. The
	// verdict is taken from this and nothing else.
	fioSize = "256M"

	// loadSize is the region the sustained load churns. Separate from fioSize
	// so an interrupted load write is never mistaken for lost data.
	loadSize = "512M"

	// rootLoadDir is where the root-filesystem load runs. The user's home is on
	// the root volume; /tmp is tmpfs on this image, so a load there would move
	// no bytes on vda and the root would never see the fault at all.
	rootLoadDir = "/home/ubuntu/.storagefault-load"

	// rootLoadSize is deliberately small. The point is journal traffic on the
	// root volume, not filling an 8 GiB disk on a nano guest.
	rootLoadSize = "128M"

	// rootDevice is the guest's root block device, whose writes are the ones
	// that decide whether the fault could reach the root filesystem.
	rootDevice = "vda"

	// rootWriteSample is the gap between the two sectors_written readings that
	// prove the root load is live. Long enough to clear a quiet moment between
	// fio's own fsyncs, short enough not to eat into the pre-fault settle.
	rootWriteSample = 3 * time.Second

	// freezeHold is how long predastore stays frozen. It has to outlast any
	// retry budget in the I/O path, or the test measures the retry rather than
	// the outage, and it has to leave fio still running when the fault lands.
	freezeHold = 90 * time.Second

	// fioSettle is how long to let fio establish real in-flight I/O before the
	// fault. A freeze that lands before the first write proves nothing.
	fioSettle = 20 * time.Second

	// recoverySettle is how long to wait after the thaw for the backend to
	// answer again before judging the guest.
	recoverySettle = 30 * time.Second
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

	freezeSet, why := freezeSetFor(fix, hostNode)
	harness.Detail(t, "instance", instanceID, "volume", volID, "guest_device", dev,
		"host_node", nodeName(hostNode), "freeze", why)

	harness.Step(t, "writing the %s verifiable region and flushing it", fioSize)
	writeVerifiableRegion(t, tgt)

	loadRuntime := fioSettle + freezeHold + recoverySettle
	harness.Step(t, "starting sustained load for %s: %s on the volume, %s on the root filesystem",
		loadRuntime, loadSize, rootLoadSize)
	startLoad(t, tgt, loadRuntime)
	startRootLoad(t, tgt, loadRuntime)
	time.Sleep(fioSettle)
	if !fioRunning(t, tgt) {
		t.Fatalf("fio exited before the fault was injected, so nothing was under load: %s", fioLog(t, tgt))
	}
	assertRootLoadIsWriting(t, tgt)

	harness.Step(t, "freezing predastore on %s for %s", nodeNames(freezeSet), freezeHold)
	freezePredastore(t, fix, freezeSet)

	observeDuringOutage(t, fix, instanceID, tgt)

	harness.Step(t, "thawing predastore and waiting %s for the backend to answer", recoverySettle)
	thawPredastore(t, fix, freezeSet)
	time.Sleep(recoverySettle)

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
	assertWorkloadIntegrity(t, tgt)
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
	// The image ships no fio and its package lists are empty, so the update is
	// required rather than defensive.
	install := "which fio || { sudo DEBIAN_FRONTEND=noninteractive apt-get update -y -q && " +
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q fio; }"
	if out, err := harness.GuestExecTimeout(tgt, install, 5*time.Minute); err != nil {
		t.Fatalf("fio is not available in the guest and could not be installed: %v\n%s", err, out)
	}
}

// verifyJob writes and later re-checks the region whose integrity decides the
// test. It runs to completion before the fault, so every block it wrote was
// acknowledged and flushed — a mismatch afterwards is lost data, never an
// interrupted write.
func verifyJob(extra string) string {
	return fmt.Sprintf(
		"fio --name=faultverify --directory=%s --size=%s --bs=64k --rw=write "+
			"--direct=1 --ioengine=libaio --iodepth=16 --numjobs=1 --end_fsync=1 "+
			"--verify=crc32c --verify_fatal=1 --group_reporting %s",
		faultMount, fioSize, extra)
}

// loadJob keeps real I/O in flight across the freeze so the fault lands on a
// busy device. It is deliberately not verified and writes its own file: a
// time-based run is cut off mid-write by design, and judging those blocks
// would report the interruption as corruption.
func loadJob(runtime time.Duration) string {
	return fmt.Sprintf(
		"fio --name=faultload --directory=%s --size=%s --bs=64k --rw=randwrite "+
			"--direct=1 --ioengine=libaio --iodepth=16 --numjobs=1 "+
			"--time_based --runtime=%ds --group_reporting",
		faultMount, loadSize, int(runtime.Seconds()))
}

// rootLoadJob drives the root filesystem so the fault lands on vda as well as
// on the attached volume. fsync=1 after every write forces an ext4 journal
// commit, which is the metadata write that aborts the journal when it fails —
// a failed read of a clean page is merely retried and proves much less.
func rootLoadJob(runtime time.Duration) string {
	return fmt.Sprintf(
		"fio --name=rootload --directory=%s --size=%s --bs=32k --rw=randwrite "+
			"--direct=0 --ioengine=psync --numjobs=2 --nrfiles=4 --fsync=1 "+
			"--time_based --runtime=%ds --group_reporting",
		rootLoadDir, rootLoadSize, int(runtime.Seconds()))
}

// startRootLoad launches the root-filesystem load detached and removes its
// files afterwards, so a nano guest's 8 GiB root is not left consumed.
func startRootLoad(t *testing.T, tgt harness.SSHTarget, runtime time.Duration) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = harness.GuestExecTimeout(tgt, "sudo rm -rf "+rootLoadDir, 60*time.Second)
	})

	cmd := fmt.Sprintf("mkdir -p %s && rm -f ~/rootfio.log; "+
		"setsid bash -c '%s > ~/rootfio.log 2>&1' >/dev/null 2>&1 &",
		rootLoadDir, rootLoadJob(runtime))
	if out, err := harness.GuestExec(tgt, cmd); err != nil {
		t.Fatalf("start root fio load: %v\n%s", err, out)
	}
}

// writeVerifiableRegion lays down the region the verdict is taken from and
// waits for it to complete. Nothing is frozen yet, so a failure here is a
// broken environment rather than evidence about the bug.
func writeVerifiableRegion(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	out, err := harness.GuestExecTimeout(tgt, "sudo "+verifyJob(""), 10*time.Minute)
	if err != nil {
		t.Fatalf("could not write the verifiable region before the fault: %v\n%s", err, out)
	}
}

// startLoad launches the sustained workload detached, so it keeps running while
// the test injects the fault. setsid survives the SSH session closing.
func startLoad(t *testing.T, tgt harness.SSHTarget, runtime time.Duration) {
	t.Helper()
	cmd := fmt.Sprintf("rm -f ~/fio.log ~/fio.done; "+
		"sudo setsid bash -c '%s > /home/ubuntu/fio.log 2>&1; echo $? > /home/ubuntu/fio.done' >/dev/null 2>&1 &",
		loadJob(runtime))
	if out, err := harness.GuestExec(tgt, cmd); err != nil {
		t.Fatalf("start fio load: %v\n%s", err, out)
	}
}

func fioRunning(t *testing.T, tgt harness.SSHTarget) bool {
	t.Helper()
	out := bestEffort(tgt, "pgrep -x fio >/dev/null && echo RUNNING || echo GONE")
	return strings.Contains(out, "RUNNING")
}

func fioLog(t *testing.T, tgt harness.SSHTarget) string {
	t.Helper()
	return bestEffort(tgt, "sudo tail -n 40 ~/fio.log")
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

// assertWorkloadIntegrity re-reads what fio wrote and checks every checksum.
// This is what distinguishes "the guest went read-only" from "the volume lost
// an acknowledged write", which are different failures with different fixes.
func assertWorkloadIntegrity(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	out, err := harness.GuestExecTimeout(tgt, "sudo "+verifyJob("--verify_only"), 10*time.Minute)
	if err != nil {
		t.Errorf("crc32c verification failed after recovery — an acknowledged write did not survive: %v\n%s", err, out)
		return
	}
	harness.Step(t, "crc32c verification passed: every acknowledged write survived")
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
func ensureFaultInstance(t *testing.T, fix *Fixture) (instanceID string, tgt harness.SSHTarget) {
	t.Helper()
	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	ami := harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	instanceID = harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     vpc.SubnetID,
		SGID:         vpc.SGID,
	})
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	host, port := harness.InstancePublicSSHHost(t, inst)

	harness.Step(t, "waiting for guest SSH on %s:%d (instance_type=%s)", host, port, instType)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", keyPath, 5*time.Minute) {
		t.Fatalf("guest %s SSH %s:%d not ready after 5m", instanceID, host, port)
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
