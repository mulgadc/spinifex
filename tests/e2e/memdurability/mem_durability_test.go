//go:build e2e

package memdurability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// memDurabilityDevice is the guest-visible attach point requested for the
	// working-set volume.
	memDurabilityDevice = "/dev/sdg"
	// memDurabilityMinMemMiB is the guest memory ceiling this suite targets.
	// The decisive condition for the data-loss shape this test reproduces is
	// a guest whose memory sits below its own write working set with no
	// swap to fall back on: at that ceiling the kernel must reclaim page
	// cache under active write pressure, which is the path a concurrent
	// writeback has been observed to lose acked writes on. This floor keeps
	// the guest below the default working set below (8 streams x 256MiB =
	// 2048MiB) so that condition actually holds.
	// DiscoverInstanceTypeAtLeastMemory resolves this to the smallest
	// catalog entry that clears it, rather than a hardcoded instance type
	// name that would silently stop matching if the catalog changes.
	memDurabilityMinMemMiB = 2048
	// memDurabilityMountpoint is where the working-set volume is mounted for
	// the whole test.
	memDurabilityMountpoint = "/mnt/memdurability"
	// memDurabilityVolumeSizeGiB must clear the working set (N streams *
	// stream size) plus ext4 metadata and journal overhead with headroom;
	// undersizing here would fail the write phase for a reason unrelated to
	// the bug under test.
	memDurabilityVolumeSizeGiB = 4

	// memDurabilityIOBaseTimeout is the fixed portion of the write and
	// cold-reread timeout budgets: SSH connection setup and shell startup,
	// independent of how much data moves.
	memDurabilityIOBaseTimeout = 30 * time.Second
	// memDurabilityIOPerMiB is the variable portion: how long one MiB is
	// budgeted, deliberately generous (well below any real deployment's
	// expected throughput) because the guest under test is memory-starved
	// by design and its writes are flowing to a network-backed volume — a
	// tight bound here would risk the exact failure this test must avoid:
	// a timeout that looks like a red durability test but proves nothing
	// about whether any stream actually lost data.
	memDurabilityIOPerMiB = 250 * time.Millisecond
)

// streamShaRE captures a "<sha256>  <path>/stream-<index>.bin" line so PRE
// and POST reads can be matched up per stream index rather than by line
// order, which SSH/dd/shell interleaving does not guarantee to preserve.
var streamShaRE = regexp.MustCompile(`([0-9a-f]{64})\s+\S*stream-(\d+)\.bin`)

// TestConcurrentWriteMemoryPressureDurability is the fail-first regression
// test for a known-open data-loss bug: on a guest whose memory sits below
// its own write working set, concurrent large writes have been observed to
// be acked by the guest and then read back as zero-filled (or otherwise
// altered) after the page cache is dropped — i.e. the write never actually
// made it durable through viperblock to the backing store, but nothing told
// the guest that at write time.
//
// This test drives N large writes issued in parallel against a
// deliberately undersized guest, forces a global sync and a cold page-cache
// drop, then rereads and compares per stream.
//
// It asserts PER-STREAM byte equality rather than an aggregate hash so a
// failure names the losing stream directly, instead of forcing whoever reads
// the failure to re-derive which of N concurrent writers lost data from a
// single combined digest.
//
// This test is EXPECTED TO FAIL: it reproduces a known-open data-loss bug,
// not a defect in the test itself. See this package's doc comment for why
// it is excluded from the default e2e run despite that.
func TestConcurrentWriteMemoryPressureDurability(t *testing.T) {
	fix := requireMemDurabilityFixture(t)
	harness.Phase(t, "Memory-Pressure Concurrent-Write Durability")

	streams := memDurabilityStreams(t)
	streamMiB := memDurabilityStreamMiB(t)
	workingSetMiB := streams * streamMiB

	instanceID, tgt := ensureMemDurabilityInstance(t, fix)
	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := harness.EnsureVolume(t, fix.Harness, az, memDurabilityVolumeSizeGiB)

	harness.Detail(t, "streams", streams, "stream_mib", streamMiB, "working_set_mib", workingSetMiB,
		"instance", instanceID, "volume", volID)

	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, memDurabilityDevice)
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)

	mkfsAndMount(t, tgt, dev)
	disableSwap(t, tgt)

	preHashes := writeStreamsParallel(t, tgt, streams, streamMiB)
	postHashes := coldRereadStreams(t, tgt, workingSetMiB)

	// Per-stream comparison, not aggregate: every stream is checked and every
	// mismatch reported, so one test run names every losing stream rather
	// than stopping at the first.
	failed := 0
	for i := 1; i <= streams; i++ {
		pre, havePre := preHashes[i]
		post, havePost := postHashes[i]
		if !havePre {
			t.Errorf("stream %d/%d: no pre-drop sha256 captured (write phase output did not include it)", i, streams)
			failed++
			continue
		}
		if !havePost {
			t.Errorf("stream %d/%d: no post-drop-caches sha256 captured (cold reread output did not include it)", i, streams)
			failed++
			continue
		}
		if pre != post {
			t.Errorf("stream %d/%d: sha256 changed across drop_caches cold reread (wrote %s, reread %s) — this stream's acked write did not survive reclaim under memory pressure",
				i, streams, pre, post)
			failed++
		}
	}

	harness.Detail(t, "streams_failed", failed, "streams_total", streams)
}

// ensureMemDurabilityInstance launches (or returns the memoized) instance
// sized to memDurabilityMinMemMiB and waits for guest SSH.
func ensureMemDurabilityInstance(t *testing.T, fix *Fixture) (instanceID string, tgt harness.SSHTarget) {
	t.Helper()
	instType, arch := harness.DiscoverInstanceTypeAtLeastMemory(t, fix.Harness, memDurabilityMinMemMiB)
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

	harness.Step(t, "waiting for guest SSH on %s:%d (instance_type=%s mem_floor=%dMiB)", host, port, instType, memDurabilityMinMemMiB)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", keyPath, 5*time.Minute) {
		t.Fatalf("guest %s SSH %s:%d not ready after 5m", instanceID, host, port)
	}
	return instanceID, harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
}

// mkfsAndMount formats dev ext4 and mounts it at memDurabilityMountpoint.
func mkfsAndMount(t *testing.T, tgt harness.SSHTarget, dev string) {
	t.Helper()
	script := strings.Join([]string{
		fmt.Sprintf("sudo mkfs.ext4 -F -L memdurability /dev/%s >/dev/null 2>&1", dev),
		fmt.Sprintf("sudo mkdir -p %s", memDurabilityMountpoint),
		fmt.Sprintf("sudo mount /dev/%s %s", dev, memDurabilityMountpoint),
	}, " && ")
	// mkfs and mount touch no guest payload data, so the default GuestExec
	// timeout budget (fixed, small) is the right fit — no scaling needed.
	out, err := harness.GuestExec(tgt, script)
	if err != nil {
		t.Fatalf("mkfsAndMount(%s): %v\n%s", dev, err, out)
	}
}

// disableSwap turns off any swap the guest image ships with. Swap gives the
// kernel a release valve under memory pressure that would mask the fault
// this test targets, so it must be off for the "no swap" condition the
// repro depends on to actually hold. Best-effort — a guest image with no
// swap configured returns a harmless non-zero exit, and failing to disable
// an already-absent swap must not fail the test for a reason unrelated to
// the bug.
func disableSwap(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	out, err := harness.GuestExec(tgt, "sudo swapoff -a")
	if err != nil {
		t.Logf("disableSwap: swapoff -a: %v (continuing — likely no swap configured)\n%s", err, out)
	}
}

// writeStreamsParallel launches n background dd writers of streamMiB each
// against the mounted volume, waits for all of them, syncs once, then reads
// back every stream's sha256 while it is still page-cache-warm. Returns the
// per-stream-index sha256 map.
//
// The streams are launched as backgrounded shell jobs within a single guest
// command rather than one command per stream: concurrency is the condition
// under test, and N sequential SSH round trips would not produce genuinely
// overlapping writes.
//
// This single command pushes the full working set through dd and then
// forces it durable with sync, so it is budgeted with memDurabilityIOTimeout
// rather than the small fixed default: on a memory-starved guest writing to
// a network-backed volume, that transfer plus its sync flush can easily
// exceed a timeout sized for a trivial payload, and a timeout failure here
// must never be mistaken for the durability failure this test exists to
// catch.
func writeStreamsParallel(t *testing.T, tgt harness.SSHTarget, n, streamMiB int) map[int]string {
	t.Helper()
	workingSetMiB := n * streamMiB
	harness.Step(t, "writing %d parallel streams of %d MiB each", n, streamMiB)

	var writers strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&writers, "sudo dd if=/dev/urandom of=%s/stream-%d.bin bs=1M count=%d status=none & ",
			memDurabilityMountpoint, i, streamMiB)
	}
	script := writers.String() + "wait; sync; sudo sha256sum " + memDurabilityMountpoint + "/stream-*.bin"

	timeout := memDurabilityIOTimeout(workingSetMiB)
	out, err := harness.GuestExecTimeout(tgt, script, timeout)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("writeStreamsParallel: TIMED OUT after %s writing %d MiB across %d streams — this is an infrastructure/throughput timeout, NOT evidence of the durability bug (raise SPINIFEX_MEMDURABILITY_STREAM_MIB's implied budget or investigate slow guest I/O separately): %v\n%s",
				timeout, workingSetMiB, n, err, out)
		}
		t.Fatalf("writeStreamsParallel: %v\n%s", err, out)
	}
	return parseStreamShas(out)
}

// coldRereadStreams drops the guest's page cache (echo 3 > drop_caches) and
// rereads every stream's sha256, forcing the read path through the backend
// rather than cached pages — the reread that must match what dd wrote in
// writeStreamsParallel for the guest's data to be considered durable.
//
// Run as its own guest command, separate from the write phase: budgeting one
// call per phase keeps each phase's timeout independent of how long the
// other phase took. workingSetMiB sizes this call's own timeout budget:
// rereading the full working set after a cache drop forces every byte back
// through the backend, which is not necessarily fast just because the
// preceding write already paid a similar cost.
func coldRereadStreams(t *testing.T, tgt harness.SSHTarget, workingSetMiB int) map[int]string {
	t.Helper()
	harness.Step(t, "dropping page cache and rereading streams cold")

	script := "echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null && sudo sha256sum " +
		memDurabilityMountpoint + "/stream-*.bin"
	timeout := memDurabilityIOTimeout(workingSetMiB)
	out, err := harness.GuestExecTimeout(tgt, script, timeout)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("coldRereadStreams: TIMED OUT after %s rereading %d MiB — this is an infrastructure/throughput timeout, NOT evidence of the durability bug: %v\n%s",
				timeout, workingSetMiB, err, out)
		}
		t.Fatalf("coldRereadStreams: %v\n%s", err, out)
	}
	return parseStreamShas(out)
}

// memDurabilityIOTimeout returns the timeout budget for a guest command that
// moves totalMiB of payload, scaling with payload size rather than using a
// fixed ceiling — the working set is env-overridable
// (SPINIFEX_MEMDURABILITY_STREAMS / SPINIFEX_MEMDURABILITY_STREAM_MIB), so a
// fixed budget sized for the default would silently become too tight the
// moment either is raised.
func memDurabilityIOTimeout(totalMiB int) time.Duration {
	return memDurabilityIOBaseTimeout + time.Duration(totalMiB)*memDurabilityIOPerMiB
}

// parseStreamShas extracts a stream-index -> sha256 map from sha256sum
// output naming files as ".../stream-<n>.bin".
func parseStreamShas(out string) map[int]string {
	result := map[int]string{}
	for _, m := range streamShaRE.FindAllStringSubmatch(out, -1) {
		idx, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		result[idx] = m[1]
	}
	return result
}

// memDurabilityStreams returns the number of parallel writers, overridable
// via SPINIFEX_MEMDURABILITY_STREAMS. Default 8 x the default 256MiB stream
// size below totals a 2048MiB working set against the 2048MiB guest memory
// floor above — sized so the working set cannot comfortably fit in page
// cache, which is the condition this test needs to hold.
func memDurabilityStreams(t *testing.T) int {
	return envPositiveIntOr(t, "SPINIFEX_MEMDURABILITY_STREAMS", 8)
}

// memDurabilityStreamMiB returns the size of each parallel writer's stream,
// overridable via SPINIFEX_MEMDURABILITY_STREAM_MIB. See memDurabilityStreams
// for how the default pairs with the guest memory floor.
func memDurabilityStreamMiB(t *testing.T) int {
	return envPositiveIntOr(t, "SPINIFEX_MEMDURABILITY_STREAM_MIB", 256)
}

// envPositiveIntOr returns the named environment variable parsed as a
// positive int, or def when unset. A malformed or non-positive value is a
// configuration error and fails the test outright via t.Fatalf, rather than
// silently falling back to the default — a run that quietly used the wrong
// working set would produce a plausible-looking result attributed to the
// wrong sizing.
//
// This is intentionally not shared with storagegrowth's helper of the same
// name: that one has no *testing.T available at its call sites and panics
// instead, whereas every call site here runs inside a test body with t
// already in hand, so failing through the test framework is both possible
// and the better fit.
func envPositiveIntOr(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q is not a positive integer", key, v)
	}
	return n
}
