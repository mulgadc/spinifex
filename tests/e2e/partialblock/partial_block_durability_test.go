//go:build e2e

package partialblock

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// partialBlockDevice is the guest-visible attach point requested for the
	// working-set volume.
	partialBlockDevice = "/dev/sdp"

	// partialBlockVolumeSizeGiB is the smallest volume that comfortably holds
	// the working region. The region itself is a few MiB at the head of the
	// device; the rest is never touched.
	partialBlockVolumeSizeGiB = 1

	// blockSize mirrors viperblock's block granularity. The read-modify-write
	// path under test runs only when a write covers part of one of these, so
	// this value has to match the engine's or the workload silently degrades
	// into full-block overwrites that exercise nothing.
	blockSize = 4096
	// halfSize splits a block into two disjoint sub-block ranges. Two writers
	// each own one half, so every write is partial by construction and the two
	// writers collide on the same block without ever overlapping each other's
	// bytes — a lost update is therefore unambiguous, not a write-write tie.
	halfSize = blockSize / 2

	// defaultBlocks and defaultRounds size the workload. Both are modest on
	// purpose: the predecessor suite moved a 2048 MiB working set, which after
	// RS(2,1) left roughly 3 GiB of backend behind per run and twice drove the
	// node under predastore's nearfull watermark, wedging every guest on it.
	// Collision probability here comes from two writers hammering the same
	// blocks in the same order, not from volume.
	defaultBlocks = 2048
	defaultRounds = 6

	// partialBlockIOBaseTimeout is the fixed part of the IO budget: SSH setup
	// and interpreter startup, independent of how much data moves.
	partialBlockIOBaseTimeout = 60 * time.Second
	// partialBlockIOPerWrite budgets one O_DIRECT sub-block write. Deliberately
	// generous: each one is a synchronous round trip to a network-backed
	// volume, and a tight bound risks the failure mode this suite must avoid —
	// a timeout that looks like a red durability test but proves nothing about
	// whether any write was lost.
	partialBlockIOPerWrite = 3 * time.Millisecond
)

// wroteRE captures the per-writer byte count each child reports, so the test
// can assert both writers moved exactly the bytes they were asked to rather
// than trusting an exit status alone.
var wroteRE = regexp.MustCompile(`WROTE half=(\d+) bytes=(\d+)`)

// checkedRE captures the verifier's tally.
var checkedRE = regexp.MustCompile(`CHECKED (\d+) BAD (\d+)`)

// partialBlockDriver is the guest-side workload. It is Python rather than a
// shell pipeline because the workload needs three things a shell cannot give:
// O_DIRECT with a correctly aligned buffer, writes at sub-block offsets that
// reach the device instead of being merged in the page cache, and a real exit
// status per writer.
//
// That last point is why the predecessor suite could not fail. It ended its
// write phase with `... & wait; sync; sha256sum`, and the status observed was
// sha256sum's; bare `wait` returns 0 unconditionally, so every writer error was
// invisible and a backend ENOSPC presented as a pass.
const partialBlockDriver = `
import mmap, os, struct, sys

BLOCK = 4096
HALF = 2048
REC = 32
MAGIC = 0x5042444B

def fill(buf, blk, half, rnd):
    # Stamp the whole half, not just a header, so a splice that reverts only
    # part of the range is still caught.
    rec = struct.pack("<IIII", MAGIC, blk, half, rnd) + b"\x00" * 16
    buf.seek(0)
    buf.write(rec * (HALF // REC))

def writer(dev, half, blocks, rounds):
    # mmap is page-aligned, which satisfies O_DIRECT's buffer alignment rule;
    # a plain bytes object would be rejected with EINVAL.
    fd = os.open(dev, os.O_WRONLY | os.O_DIRECT)
    try:
        buf = mmap.mmap(-1, HALF)
        total = 0
        for rnd in range(1, rounds + 1):
            for blk in range(blocks):
                fill(buf, blk, half, rnd)
                n = os.pwrite(fd, buf, blk * BLOCK + half * HALF)
                if n != HALF:
                    raise IOError("short write %d want %d at blk %d half %d" % (n, HALF, blk, half))
                total += n
        os.fsync(fd)
    finally:
        os.close(fd)
    return total

def do_write(dev, blocks, rounds):
    pids = []
    for half in (0, 1):
        pid = os.fork()
        if pid == 0:
            try:
                total = writer(dev, half, blocks, rounds)
            except Exception as e:
                os.write(2, ("writer %d failed: %s\n" % (half, e)).encode())
                os._exit(1)
            os.write(1, ("WROTE half=%d bytes=%d\n" % (half, total)).encode())
            os._exit(0)
        pids.append(pid)
    rc = 0
    for pid in pids:
        _, status = os.waitpid(pid, 0)
        if status != 0:
            rc = 1
    return rc

def do_verify(dev, blocks, rounds):
    fd = os.open(dev, os.O_RDONLY | os.O_DIRECT)
    buf = mmap.mmap(-1, BLOCK)
    bad = 0
    checked = 0
    try:
        for blk in range(blocks):
            n = os.preadv(fd, [buf], blk * BLOCK)
            if n != BLOCK:
                os.write(2, ("short read %d at blk %d\n" % (n, blk)).encode())
                return 1
            for half in (0, 1):
                # First and last record of the half: the observed failure
                # reverts the whole spliced range, but checking both ends also
                # catches a partial revert.
                for pos in (0, HALF - REC):
                    off = half * HALF + pos
                    magic, gblk, ghalf, grnd = struct.unpack_from("<IIII", buf, off)
                    checked += 1
                    if magic != MAGIC or gblk != blk or ghalf != half or grnd != rounds:
                        bad += 1
                        if bad <= 20:
                            os.write(1, ("MISMATCH blk=%d half=%d off=%d magic=0x%x got_blk=%d got_half=%d got_round=%d want_round=%d\n" % (
                                blk, half, pos, magic, gblk, ghalf, grnd, rounds)).encode())
    finally:
        os.close(fd)
    os.write(1, ("CHECKED %d BAD %d\n" % (checked, bad)).encode())
    return 1 if bad else 0

mode = sys.argv[1]
dev = sys.argv[2]
blocks = int(sys.argv[3])
rounds = int(sys.argv[4])
sys.exit(do_write(dev, blocks, rounds) if mode == "write" else do_verify(dev, blocks, rounds))
`

// TestPartialBlockConcurrentWriteDurability drives two concurrent O_DIRECT
// writers into disjoint halves of the same 4096-byte blocks on a raw device,
// then asserts after a cold reread that every half carries the final round
// stamp.
//
// The engine reaches its read-modify-write path only for a partial block
// (viperblock's write path branches on writeStart > 0 || writeEnd < blockSize);
// a fully covered block takes a full-overwrite branch that reads nothing and
// can lose nothing. Two writers that both read generation N, each splice their
// own half, and let the higher sequence number win wholesale is the shape of
// the lost update this asserts against — so one half reading back at an
// earlier round is the signature.
//
// The device is raw and unformatted on purpose. A filesystem defeats this
// workload: it coalesces writes into aligned full-block requests, which is how
// the predecessor suite ended up issuing zero sub-4096 and zero unaligned
// requests and passing on a build that still carried the bug.
func TestPartialBlockConcurrentWriteDurability(t *testing.T) {
	fix := requirePartialBlockFixture(t)
	harness.Phase(t, "Partial-Block Concurrent-Write Durability")

	blocks := envInt(t, "SPINIFEX_PARTIALBLOCK_BLOCKS", defaultBlocks)
	rounds := envInt(t, "SPINIFEX_PARTIALBLOCK_ROUNDS", defaultRounds)
	writesPerWriter := blocks * rounds
	wantBytes := writesPerWriter * halfSize

	instanceID, tgt := ensurePartialBlockInstance(t, fix)
	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := harness.EnsureVolume(t, fix.Harness, az, partialBlockVolumeSizeGiB)

	harness.Detail(t, "blocks", blocks, "rounds", rounds, "region_mib", blocks*blockSize/(1<<20),
		"instance", instanceID, "volume", volID)

	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, partialBlockDevice)
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)

	// Detach before the fixture's own cleanup deletes the volume. DeleteVolume
	// against a still-attached volume is what leaks volumes on teardown, and
	// the delete itself is left to the fixture so the volume is released
	// exactly once.
	t.Cleanup(func() { harness.DetachVolumeWait(t, fix.AWS, volID) })

	installDriver(t, tgt)

	budget := partialBlockIOBaseTimeout + time.Duration(writesPerWriter)*partialBlockIOPerWrite
	harness.Step(t, "writing %d rounds x %d blocks x 2 halves O_DIRECT to /dev/%s (budget %s)", rounds, blocks, dev, budget)
	out, err := harness.GuestExecTimeout(tgt,
		fmt.Sprintf("sudo python3 /tmp/partialblock.py write /dev/%s %d %d", dev, blocks, rounds), budget)
	if err != nil {
		t.Fatalf("write phase failed (this is a writer error or timeout, NOT evidence of a lost update): %v\n%s", err, out)
	}

	// Assert both writers moved exactly the bytes asked of them. Without this a
	// writer that failed early could leave the device holding a consistent but
	// stale round, and the verify phase would agree with itself.
	assertWriterBytes(t, out, wantBytes)

	harness.Step(t, "dropping page cache")
	if out, err := harness.GuestExec(tgt, "sudo sync && echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null"); err != nil {
		t.Fatalf("drop_caches: %v\n%s", err, out)
	}

	harness.Step(t, "cold O_DIRECT reread, asserting every half carries round %d", rounds)
	out, err = harness.GuestExecTimeout(tgt,
		fmt.Sprintf("sudo python3 /tmp/partialblock.py verify /dev/%s %d %d", dev, blocks, rounds), budget)
	if err != nil {
		// A non-zero verify exit is the durability failure itself, so the
		// output carries the per-block detail and must be surfaced whole.
		t.Fatalf("cold reread found lost updates — a partial write was acked and did not survive:\n%s", out)
	}

	checked, bad := parseChecked(t, out)
	if bad != 0 {
		t.Errorf("verify reported %d bad halves out of %d checked", bad, checked)
	}
	if want := blocks * 2 * 2; checked != want {
		t.Errorf("verify checked %d records, want %d — the verifier did not cover the whole region", checked, want)
	}
	harness.Detail(t, "records_checked", checked, "records_bad", bad)
}

// ensurePartialBlockInstance launches (or returns the memoized) guest and waits
// for SSH. Guest size is deliberately not a parameter: the memory-pressure
// trigger model the predecessor suite encoded was retracted — the fault
// reproduces with a single sequential writer on a guest with memory to spare —
// so the smallest catalog entry is the right choice here.
func ensurePartialBlockInstance(t *testing.T, fix *Fixture) (instanceID string, tgt harness.SSHTarget) {
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

// installDriver writes the guest-side workload to /tmp and confirms python3 can
// parse it, so a syntax error surfaces here rather than as an opaque non-zero
// exit in the middle of the write phase.
func installDriver(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	script := fmt.Sprintf("cat > /tmp/partialblock.py <<'PBEOF'\n%s\nPBEOF\npython3 -c 'import py_compile,sys; py_compile.compile(\"/tmp/partialblock.py\", doraise=True)'", partialBlockDriver)
	if out, err := harness.GuestExec(tgt, script); err != nil {
		t.Fatalf("installDriver: %v\n%s", err, out)
	}
}

// assertWriterBytes checks that both halves reported completion and that each
// moved exactly wantBytes.
func assertWriterBytes(t *testing.T, out string, wantBytes int) {
	t.Helper()
	seen := map[int]int{}
	for _, m := range wroteRE.FindAllStringSubmatch(out, -1) {
		half, _ := strconv.Atoi(m[1])
		got, _ := strconv.Atoi(m[2])
		seen[half] = got
	}
	for half := 0; half < 2; half++ {
		got, ok := seen[half]
		if !ok {
			t.Fatalf("writer half=%d reported no byte count — it did not run to completion:\n%s", half, out)
		}
		if got != wantBytes {
			t.Fatalf("writer half=%d wrote %d bytes, want %d — short write path, not a durability result:\n%s", half, got, wantBytes, out)
		}
	}
}

// parseChecked pulls the verifier's tally out of its output.
func parseChecked(t *testing.T, out string) (checked, bad int) {
	t.Helper()
	m := checkedRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("verify produced no CHECKED line — cannot tell whether it examined anything:\n%s", out)
	}
	checked, _ = strconv.Atoi(m[1])
	bad, _ = strconv.Atoi(m[2])
	return checked, bad
}

// envInt reads a positive integer override, failing outright on a malformed
// value rather than silently falling back to the default.
func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q is not a positive integer", key, v)
	}
	return n
}
