//go:build e2e

package storagegrowth

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	// churnDevice is the guest-visible attach point requested for the single
	// long-lived attach this workload performs. As with the N-pass workload the
	// kernel may enumerate a different name, so the actual device is rediscovered
	// via GuestDiskSet/WaitForNewGuestDisk rather than trusting this value.
	churnDevice = "/dev/sdf"
	// churnVolumeSizeGiB sizes the volume the churn writes into. It only needs to
	// be large enough to hold every parallel writer's region; the workload
	// deliberately rewrites a small footprint many times rather than growing it.
	churnVolumeSizeGiB = 4

	// churnDurationEnv sets how long the raw-overwrite loop runs, in seconds.
	// The default spans more than two 5-minute GC ticks so a sweep provably
	// fires while the volume is still open — the case the detach-per-pass N-pass
	// workload cannot produce, because its sweeps only ever land at reopen.
	churnDurationEnv = "SPINIFEX_CHURN_DURATION_SECONDS"
	// churnRegionMiBEnv sets the per-writer region size. Small by design: a
	// tight region rewritten repeatedly maximises chunk supersession per byte
	// written, which is what makes reclaim (or its absence) show up sharply.
	churnRegionMiBEnv = "SPINIFEX_CHURN_REGION_MIB"
	// churnParallelismEnv sets how many concurrent raw writers run against
	// distinct offset regions. Concurrency is the lever that pushes the backend
	// PutObject rate past predastore's burst=500 rate limiter, which is what
	// exercises the 503 SlowDown retry path rather than the genuine-full 507.
	churnParallelismEnv = "SPINIFEX_CHURN_PARALLELISM"
)

// TestContinuousChurn attaches one volume once and drives a sustained,
// concurrent raw-device overwrite loop against it for several minutes WITHOUT
// detaching between writes, then detaches.
//
// It is deliberately a different shape from TestNPassOverwrite. That workload
// detaches after every pass, so viperblock's drain/GC only ever runs at
// reopen — it can never show whether GC reclaims superseded chunks while a
// volume stays open, and its low, serial write rate never approaches the
// predastore rate limiter's burst. This workload holds a single long open and
// writes concurrently, so it exercises two things that one cannot:
//
//   - the 503 SlowDown retry path: parallel writers push the backend PutObject
//     rate past predastore's burst=500 limiter, so the plugin must ride out
//     503s without wedging (the fix that maps only 507→ErrNoSpace).
//   - live GC reclaim: the loop spans more than two 5-minute GC ticks, so a
//     sweep provably runs while the volume is open and superseded chunks are
//     eligible to reclaim.
//
// It asserts only that the churn ran and the volume detached cleanly; the
// backend-side signals (no "backend out of space" latch, GC "sweep complete"
// swept counts) are read out of band from the viperblockd log stream, because
// the plugin's own accounting is not visible to the guest.
func TestContinuousChurn(t *testing.T) {
	fix := requireStorageGrowthFixture(t)
	harness.Phase(t, "Continuous Churn — single long-open concurrent raw overwrite (503 limiter + live GC)")

	duration := churnDuration()
	regionMiB := churnRegionMiB()
	parallelism := churnParallelism()

	instanceID, tgt := ensureWorkloadInstance(t, fix)
	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := createWorkloadVolume(t, fix, az, false)

	harness.Detail(t, "duration_s", int(duration.Seconds()), "region_mib", regionMiB,
		"parallelism", parallelism, "instance", instanceID, "volume", volID)

	// One attach for the whole run — this is the load-bearing difference from
	// the N-pass workload. Discover the actual kernel device name rather than
	// trusting churnDevice.
	before := harness.GuestDiskSet(t, tgt)
	harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, churnDevice)
	dev := harness.WaitForNewGuestDisk(t, tgt, before, 90*time.Second)

	// Detach unconditionally when the churn is done, even on failure, so a wedged
	// or slow run does not strand the volume attached to the instance. Registered
	// first so it runs LAST (t.Cleanup is LIFO): the writer-kill below must run
	// before the detach, or a still-running dd keeps the device busy and the
	// detach fails — which is exactly how an earlier version leaked a volume.
	t.Cleanup(func() { harness.DetachVolumeWait(t, fix.AWS, volID) })
	// Belt-and-suspenders to the guest-side timeout/trap in runContinuousChurn:
	// make sure no writer survives the test, even if the SSH command was killed
	// before its own trap could fire. Registered second so it runs FIRST.
	t.Cleanup(func() { killGuestChurn(tgt, dev) })

	passes := runContinuousChurn(t, tgt, dev, duration, regionMiB, parallelism)
	require.Positivef(t, passes, "guest reported 0 overwrite passes over %s — the churn never wrote, so nothing was stressed", duration)

	// Every mark is one 4 MiB chunk write, so total bytes written is passes×4 MiB
	// regardless of region size or writer count.
	harness.Detail(t, "chunk_writes", passes,
		"approx_bytes_written_gib",
		fmt.Sprintf("%.1f", float64(passes)*4.0/1024.0))
}

// runContinuousChurn runs parallelism concurrent raw-overwrite loops against
// distinct 4 MiB-aligned regions of /dev/<dev> for the given duration, and
// returns the total number of 4 MiB chunk writes the writers completed.
//
// Each writer owns one region and rewrites it a chunk at a time, so every writer
// supersedes its own chunks continuously — maximising the superseded backlog GC
// must reclaim. Writes use oflag=direct so they bypass the guest page cache and
// hit the NBD/viperblock path immediately.
//
// Bounding and containment matter as much as the workload here. Each dd writes
// exactly one 4 MiB chunk (count=1) and the clock is re-checked after every
// write, so no single dd can run long and the loop stops promptly at the
// deadline — an earlier version wrote whole regions per dd and only checked the
// clock between them, so a slow write overran the deadline badly. The whole loop
// runs as root under one `sudo bash` with a process-group kill trap, and is
// additionally wrapped in a guest-side `timeout`: if the SSH command is killed
// or the deadline slips, the trap (or timeout) reaps every writer. This is
// load-bearing — the writers are root (they open a raw device), so a non-root
// shell cannot signal them; only a root-owned process group can, which is why
// the trap lives inside `sudo bash` rather than around it.
func runContinuousChurn(t *testing.T, tgt harness.SSHTarget, dev string, duration time.Duration, regionMiB, parallelism int) int64 {
	t.Helper()
	harness.Step(t, "%s of %d×%d MiB concurrent raw overwrites against /dev/%s",
		duration, parallelism, regionMiB, dev)

	// span is the region size in whole 4 MiB chunks; each writer j owns the span
	// starting at chunk j*span, so writers never overlap.
	span := regionMiB / 4
	require.Positivef(t, span, "region_mib=%d is smaller than the 4MiB chunk — nothing to write", regionMiB)

	// The inner workload, run as root so the kill trap can actually reap the
	// (root) dd writers. `kill 0` on TERM/INT/HUP signals the whole process
	// group, so a timeout or a dropped SSH connection tears every writer down
	// rather than orphaning it against the raw device.
	inner := strings.Join([]string{
		`trap 'kill 0' TERM INT HUP`,
		`marks=$(mktemp)`,
		fmt.Sprintf(`end=$(( $(date +%%s) + %d ))`, int(duration.Seconds())),
		fmt.Sprintf(`span=%d`, span),
		fmt.Sprintf(`for j in $(seq 0 %d); do`, parallelism-1),
		`  ( base=$(( j * span ));`,
		`    while [ "$(date +%s)" -lt "$end" ]; do`,
		`      k=0;`,
		`      while [ "$k" -lt "$span" ]; do`,
		fmt.Sprintf(`        dd if=/dev/zero of=/dev/%s bs=4M count=1 seek=$(( base + k )) oflag=direct conv=notrunc status=none && printf x >> "$marks";`, dev),
		`        k=$(( k + 1 ));`,
		`        [ "$(date +%s)" -ge "$end" ] && break;`,
		`      done;`,
		`    done ) &`,
		`done`,
		`wait`,
		`wc -c < "$marks"`,
		`rm -f "$marks"`,
	}, "\n")

	// Wrap the root loop in a guest-side timeout so the guest self-limits even if
	// the SSH channel misbehaves: SIGTERM at deadline+30s (fires the trap),
	// SIGKILL 20s later if anything is still alive. single-quote the inner script
	// for `sudo bash -c` and escape embedded quotes for the outer shell.
	guestTimeout := int(duration.Seconds()) + 30
	script := fmt.Sprintf(`timeout -k 20 %d sudo bash -c %s`, guestTimeout, shSingleQuote(inner))

	// The SSH budget sits outside the guest timeout with margin for the kill-after
	// window plus teardown; the guest, not SSH, is the primary bound now.
	budget := duration + 3*time.Minute
	out, err := harness.GuestExecTimeout(tgt, script, budget)
	require.NoErrorf(t, err, "continuous churn loop on /dev/%s:\n%s", dev, out)

	passes, perr := strconv.ParseInt(strings.TrimSpace(lastLine(out)), 10, 64)
	require.NoErrorf(t, perr, "parse chunk-write count from churn output %q", out)
	return passes
}

// killGuestChurn best-effort reaps any churn dd still writing to /dev/<dev>,
// so a detach that follows cannot fail on a busy device. It is intentionally
// forgiving: the guest may already be gone, or there may be nothing to kill.
func killGuestChurn(tgt harness.SSHTarget, dev string) {
	cmd := fmt.Sprintf(`sudo pkill -9 -f "of=/dev/%s" || true`, dev)
	_, _ = harness.GuestExecTimeout(tgt, cmd, 30*time.Second)
}

// shSingleQuote wraps s in single quotes for safe use as one shell word,
// escaping any embedded single quotes via the '\” idiom.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// lastLine returns the last non-empty line of s, so the pass count survives any
// stray sudo/ssh noise printed ahead of it.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// churnDuration returns how long the overwrite loop runs, overridable via
// churnDurationEnv. The default is 11 minutes so the run spans at least two
// full 5-minute GC ticks with margin.
func churnDuration() time.Duration {
	return time.Duration(envPositiveIntOr(churnDurationEnv, 11*60)) * time.Second
}

// churnRegionMiB returns the per-writer region size, overridable via
// churnRegionMiBEnv. The default of 64 MiB is a tight region rewritten many
// times — high supersession density, small footprint.
func churnRegionMiB() int {
	return envPositiveIntOr(churnRegionMiBEnv, 64)
}

// churnParallelism returns the number of concurrent writers, overridable via
// churnParallelismEnv. The default of 8 pushes the backend PutObject rate well
// past predastore's burst=500 limiter on a network-backed volume.
func churnParallelism() int {
	return envPositiveIntOr(churnParallelismEnv, 8)
}
