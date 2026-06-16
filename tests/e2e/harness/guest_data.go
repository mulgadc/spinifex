//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Guest-side data-durability helpers shared by the P0 e2e tests (volume
// durability, snapshot restore, CreateImage). They drive a real guest over SSH
// to format, write, sync, and checksum bytes so the assembled
// QEMU↔viperblock↔predastore I/O path is exercised end-to-end — something the
// per-layer unit tests cannot cover.

// guestSentinelFile is the file written into a freshly formatted data volume.
const guestSentinelFile = "e2e-sentinel.bin"

// guestExecTimeout bounds a single guest command (mkfs/dd are sub-second on the
// small payloads used here; the ceiling only guards against a hung SSH).
const guestExecTimeout = 2 * time.Minute

// sha256RE matches a bare sha256 digest so the checksum can be lifted out of
// command output that may also carry the file path (`<sha>␠␠<path>`).
var sha256RE = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

// GuestExec runs cmd over SSH against tgt and returns combined stdout+stderr
// plus the run error. It never calls t.Fatal, so callers can branch on an
// expected non-zero exit.
func GuestExec(tgt SSHTarget, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), guestExecTimeout)
	defer cancel()
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		cmd,
	}
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	return string(out), err
}

// guestDiskSet returns the set of whole-disk device names (lsblk TYPE=disk)
// currently visible in the guest, keyed by bare name (e.g. "vda", "vdc").
func guestDiskSet(tgt SSHTarget) (map[string]struct{}, error) {
	out, err := GuestExec(tgt, "lsblk -dn -o NAME,TYPE")
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w\n%s", err, out)
	}
	set := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "disk" {
			set[f[0]] = struct{}{}
		}
	}
	return set, nil
}

// GuestDiskSet is the t.Fatal-on-error wrapper around guestDiskSet, used to
// snapshot the guest's disk inventory before an AttachVolume.
func GuestDiskSet(t *testing.T, tgt SSHTarget) map[string]struct{} {
	t.Helper()
	set, err := guestDiskSet(tgt)
	if err != nil {
		t.Fatalf("GuestDiskSet: %v", err)
	}
	return set
}

// WaitForNewGuestDisk polls the guest until a whole-disk device absent from
// `before` appears, returning its bare name (e.g. "vdc"). Proves a hotplugged
// volume actually reached the guest kernel, not just the EC2 control plane.
func WaitForNewGuestDisk(t *testing.T, tgt SSHTarget, before map[string]struct{}, timeout time.Duration) string {
	t.Helper()
	var found string
	EventuallyErr(t, func() error {
		now, err := guestDiskSet(tgt)
		if err != nil {
			return err
		}
		for name := range now {
			if _, ok := before[name]; !ok {
				found = name
				return nil
			}
		}
		names := make([]string, 0, len(now))
		for name := range now {
			names = append(names, name)
		}
		return fmt.Errorf("no new disk yet (visible: %s)", strings.Join(names, ","))
	}, timeout, 2*time.Second)
	return found
}

// GuestFormatWriteSentinel formats /dev/<dev> as ext4 with the given label,
// mounts it, writes sizeMiB of random data to the sentinel file, fsyncs, and
// returns the file's sha256. Unmounts before returning so the volume can be
// detached cleanly.
func GuestFormatWriteSentinel(t *testing.T, tgt SSHTarget, dev, label string, sizeMiB int) string {
	t.Helper()
	mnt := "/mnt/" + label
	script := strings.Join([]string{
		fmt.Sprintf("sudo mkfs.ext4 -F -L %s /dev/%s >/dev/null 2>&1", label, dev),
		fmt.Sprintf("sudo mkdir -p %s", mnt),
		fmt.Sprintf("sudo mount /dev/%s %s", dev, mnt),
		fmt.Sprintf("sudo dd if=/dev/urandom of=%s/%s bs=1M count=%d conv=fsync status=none", mnt, guestSentinelFile, sizeMiB),
		"sync",
		fmt.Sprintf("sudo sha256sum %s/%s", mnt, guestSentinelFile),
		fmt.Sprintf("sudo umount %s", mnt),
	}, " && ")
	out, err := GuestExec(tgt, script)
	if err != nil {
		t.Fatalf("GuestFormatWriteSentinel(%s,%s): %v\n%s", dev, label, err, out)
	}
	return mustSha(t, out)
}

// GuestReadSentinelSha mounts source (a device path such as "/dev/vdc" or a
// "/dev/disk/by-label/<label>" path) at a temp mountpoint, returns the
// sentinel file's sha256, and unmounts.
func GuestReadSentinelSha(t *testing.T, tgt SSHTarget, source, label string) string {
	t.Helper()
	mnt := "/mnt/" + label
	script := strings.Join([]string{
		fmt.Sprintf("sudo mkdir -p %s", mnt),
		fmt.Sprintf("sudo mount %s %s", source, mnt),
		fmt.Sprintf("sudo sha256sum %s/%s", mnt, guestSentinelFile),
		fmt.Sprintf("sudo umount %s", mnt),
	}, " && ")
	out, err := GuestExec(tgt, script)
	if err != nil {
		t.Fatalf("GuestReadSentinelSha(%s): %v\n%s", source, err, out)
	}
	return mustSha(t, out)
}

// GuestWriteFileSentinel writes sizeMiB of random data to an arbitrary path on
// an already-mounted filesystem (e.g. the root fs for the CreateImage test),
// fsyncs, and returns its sha256. No mkfs/mount — the path's filesystem must
// already exist.
func GuestWriteFileSentinel(t *testing.T, tgt SSHTarget, path string, sizeMiB int) string {
	t.Helper()
	script := strings.Join([]string{
		fmt.Sprintf("sudo dd if=/dev/urandom of=%s bs=1M count=%d conv=fsync status=none", path, sizeMiB),
		"sync",
		fmt.Sprintf("sudo sha256sum %s", path),
	}, " && ")
	out, err := GuestExec(tgt, script)
	if err != nil {
		t.Fatalf("GuestWriteFileSentinel(%s): %v\n%s", path, err, out)
	}
	return mustSha(t, out)
}

// GuestFileSha returns the sha256 of an existing file path in the guest.
func GuestFileSha(t *testing.T, tgt SSHTarget, path string) string {
	t.Helper()
	out, err := GuestExec(tgt, fmt.Sprintf("sudo sha256sum %s", path))
	if err != nil {
		t.Fatalf("GuestFileSha(%s): %v\n%s", path, err, out)
	}
	return mustSha(t, out)
}

// mustSha extracts the single sha256 digest from command output, failing the
// test if none is present.
func mustSha(t *testing.T, out string) string {
	t.Helper()
	sha := sha256RE.FindString(out)
	if sha == "" {
		t.Fatalf("no sha256 digest in guest output:\n%s", out)
	}
	return sha
}
