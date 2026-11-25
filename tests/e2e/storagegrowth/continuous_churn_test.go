//go:build e2e

package storagegrowth

import (
	"context"
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
	// It is also the denominator of the host WAL bound, so it is passed to
	// CreateVolume rather than assumed: a constant the creation does not use
	// makes that assertion wrong by whatever the two differ by.
	churnVolumeSizeGiB = 4
	// churnWALBudgetRatio is how many times the volume's logical size its local
	// WAL may hold on the host after the churn. A reclaimed volume retains only
	// the open generation and whatever is in flight, so 2 is slack rather than
	// a budget. It is also the discrimination threshold: the run has to write
	// past it before an unreclaimed WAL would exceed it, so raising it makes
	// the assertion need a proportionally longer run to mean anything.
	churnWALBudgetRatio = 2

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
// It also bounds the volume's WAL on the host, which is the signal the guest
// cannot produce: the host stores the guest's writes, not its data, so a fixed
// volume size caps nothing there. The remaining backend-side signals (no
// "backend out of space" latch, GC "sweep complete" swept counts) are still
// read out of band from the viperblockd log stream.
func TestContinuousChurn(t *testing.T) {
	fix := requireStorageGrowthFixture(t)
	harness.Phase(t, "Continuous Churn — single long-open concurrent raw overwrite (503 limiter + live GC)")

	duration := churnDuration()
	regionMiB := churnRegionMiB()
	parallelism := churnParallelism()

	instanceID, tgt := ensureWorkloadInstance(t, fix)
	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	volID := createWorkloadVolume(t, fix, az, churnVolumeSizeGiB, false)

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
	writtenBytes := passes * 4 * 1024 * 1024
	harness.Detail(t, "chunk_writes", passes,
		"approx_bytes_written_gib",
		fmt.Sprintf("%.1f", float64(passes)*4.0/1024.0))

	assertHostWALBounded(t, fix, volID, writtenBytes)
}

// assertHostWALBounded is the assertion this workload was missing. It drove
// exactly the write pattern that filled a production node, a small volume
// rewritten continuously, and then checked only that the churn ran and the
// volume detached, because the plugin's accounting is not visible to the guest.
//
// The WAL records writes, not data, so a volume's size bounds neither. A guest
// rewriting the same blocks produces unbounded write volume against constant
// data, and a WAL that is never reclaimed keeps all of it. Measuring the host
// directory after a run that wrote many times the volume's size is what tells
// those two apart: reclaimed stays near zero, unreclaimed tracks writtenBytes.
func assertHostWALBounded(t *testing.T, fix *Fixture, volID string, writtenBytes int64) {
	t.Helper()

	ssh := harness.NewPeerSSH()
	walBytes, dir, err := hostVolumeWALBytes(ssh, fix.Env.WANHost, volID)
	if err != nil {
		// Not a skip dressed as a pass: the measurement is the point, so a
		// node we cannot read is a failure of the test's own preconditions.
		t.Fatalf("could not measure the host WAL for %s on %s: %v", volID, fix.Env.WANHost, err)
	}

	volumeBytes := int64(churnVolumeSizeGiB) * 1024 * 1024 * 1024
	budget := volumeBytes * churnWALBudgetRatio
	harness.Detail(t, "wal_dir", dir, "wal_bytes", walBytes,
		"written_bytes", writtenBytes, "budget_bytes", budget,
		"wal_over_volume_ratio", fmt.Sprintf("%.2f", float64(walBytes)/float64(volumeBytes)))

	// The bound only discriminates once the run has written past it. Below
	// that, an unreclaimed WAL holding every byte still fits the budget and
	// the assertion passes without having tested anything, so a short or slow
	// run is inconclusive evidence and is reported as a failure rather than a
	// green tick. Raise SPINIFEX_STORAGEGROWTH_CHURN_SECS to clear it.
	require.Greaterf(t, writtenBytes, budget,
		"the churn wrote %d bytes against a %d byte budget, so an unreclaimed WAL would have passed this assertion. "+
			"The run is too short to tell reclaim from no reclaim; it is not evidence that the bound holds.",
		writtenBytes, budget)

	require.LessOrEqualf(t, walBytes, budget,
		"local WAL for %s holds %d bytes after the guest wrote %d bytes into a %d GiB volume. "+
			"A WAL that tracks bytes written rather than bytes stored is not being reclaimed, "+
			"which is what took a production node to zero bytes free. Measured at %s on %s.",
		volID, walBytes, writtenBytes, churnVolumeSizeGiB, dir, fix.Env.WANHost)
}

// hostVolumeWALBytes returns the bytes a volume's local WAL occupies on the
// node at wanHost, and the directory it measured.
//
// The root is read from the serving nbdkit process rather than assumed. base_dir
// and wal_base_dir are independent config keys (runtime/compute/nbd.go passes
// both to the plugin), so a deployment that moves the WAL to its own device
// would otherwise be measured on the wrong filesystem and report a clean zero.
// Taking it from the live command line means this measures whatever the node is
// actually serving the volume from.
func hostVolumeWALBytes(ssh *harness.PeerSSH, wanHost, volID string) (int64, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// One nbdkit per volume, so matching volume= is unambiguous; the last match
	// wins if a stale process is still exiting, because the newest is the one
	// serving the attachment. Both greps run under sudo: the daemon runs as
	// another user, and a host with hidepid would otherwise read as "no such
	// volume" rather than as a permission problem.
	readArgs := fmt.Sprintf(
		`p=$(sudo grep -lFzx 'volume=%s' /proc/[0-9]*/cmdline 2>/dev/null | tail -1); `+
			`[ -n "$p" ] && sudo grep -azE '^(base_dir|wal_base_dir)=' "$p" | tr '\0' '\n'`,
		volID)
	out, err := ssh.Run(ctx, wanHost, readArgs)
	if err != nil {
		return 0, "", fmt.Errorf("read nbdkit args for %s via PeerSSH: %w", volID, err)
	}

	var baseDir, walBaseDir string
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || v == "" {
			continue
		}
		switch k {
		case "base_dir":
			baseDir = v
		case "wal_base_dir":
			walBaseDir = v
		}
	}

	root := walBaseDir
	if root == "" {
		root = baseDir
	}
	if root == "" {
		return 0, "", fmt.Errorf("no nbdkit process on %s serves volume=%s, so the WAL root is unknown", wanHost, volID)
	}
	dir := fmt.Sprintf("%s/%s/wal", strings.TrimRight(root, "/"), volID)

	// Unpiped, so du's exit status survives. Piping it through cut discards
	// that status, and du reports a partial total on an unreadable subtree, so
	// an incomplete measurement would satisfy the bound and read as a pass.
	duOut, duErr := ssh.Run(ctx, wanHost, fmt.Sprintf("sudo du -s --block-size=1 %s", dir))
	if duErr != nil {
		return 0, dir, fmt.Errorf("du %s via PeerSSH: %w", dir, duErr)
	}
	v, perr := parseFirstIntField(string(duOut))
	if perr != nil {
		return 0, dir, fmt.Errorf("parse du output %q: %w", duOut, perr)
	}
	return v, dir, nil
}

// parseFirstIntField reads du's total out of "<bytes>\t<path>". The last field
// is the path, so parseLastIntField cannot be reused here.
func parseFirstIntField(out string) (int64, error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("no fields in %q", out)
	}
	return strconv.ParseInt(fields[0], 10, 64)
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

// TestParseFirstIntField covers the field du's total actually occupies. Piping
// du through cut discarded its exit status, so the measurement now parses the
// unpiped output, where the last field is the path rather than a number.
func TestParseFirstIntField(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want int64
		err  bool
	}{
		{name: "du output", out: "1234567\t/var/lib/spinifex/viperblock/vol-abc/wal\n", want: 1234567},
		{name: "path ending in digits", out: "42\t/var/lib/spinifex/vol-00030214\n", want: 42},
		{name: "empty", out: "   \n", err: true},
		{name: "non-numeric first field", out: "du: cannot read directory\n", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFirstIntField(tc.out)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
