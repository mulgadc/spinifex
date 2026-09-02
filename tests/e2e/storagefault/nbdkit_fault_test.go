//go:build e2e

package storagefault

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// nbdkitComm is the exact /proc/<pid>/comm for an nbdkit process, short enough
// that Linux's 15-byte truncation never applies.
const nbdkitComm = "nbdkit"

// nbdkitPluginSuffix identifies our own plugin in a discovered argv, so an
// nbdkit serving something else is never signalled.
const nbdkitPluginSuffix = "viperblock-plugin.so"

// nbdkitSettle is how long to wait after signalling nbdkit before judging the
// guest, so an assertion cannot race the signal.
const nbdkitSettle = 5 * time.Second

// nbdkitPIDForVolume resolves the pid of the nbdkit serving exactly volumeID on
// node. It matches on three things together — the comm, our plugin, and the
// volume argument — because every other nbdkit on these hosts belongs to
// another volume or another tenant, and a name match would hit all of them.
// Returns 0 with no error when nothing serves that volume here.
func nbdkitPIDForVolume(ctx context.Context, ssh harness.SSH, node harness.Node, volumeID string) (int, error) {
	// pgrep -x on the comm, then filter each candidate's own cmdline. Done on
	// the far side because one round trip per pid over SSH is needlessly slow.
	script := fmt.Sprintf(
		`for p in $(pgrep -x %s 2>/dev/null); do `+
			`c=$(tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null); `+
			`case "$c" in *%s*volume=%s*|*volume=%s*%s*) echo "$p"; esac; `+
			`done`,
		nbdkitComm, nbdkitPluginSuffix, volumeID, volumeID, nbdkitPluginSuffix)

	raw, err := ssh.Run(ctx, node, script)
	if err != nil {
		return 0, fmt.Errorf("scan nbdkit for %s on %s: %w", volumeID, node.Name, err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, nil
	}
	if len(fields) > 1 {
		return 0, fmt.Errorf("refusing to signal: %d nbdkit processes serve %s on %s (%v)",
			len(fields), volumeID, node.Name, fields)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("nbdkit pid for %s on %s was %q, not a number", volumeID, node.Name, fields[0])
	}
	return pid, nil
}

// requireNbdkitForVolume resolves the pid and fails the test when nothing is
// serving the volume. A test that signalled nothing would pass for the wrong
// reason, so the absence is fatal rather than skipped.
func requireNbdkitForVolume(t *testing.T, f *Fixture, node harness.Node, volumeID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid, err := nbdkitPIDForVolume(ctx, f.SSH, node, volumeID)
	if err != nil {
		t.Fatalf("cannot resolve nbdkit for %s on %s: %v", volumeID, node.Name, err)
	}
	if pid == 0 {
		t.Fatalf("no nbdkit serves %s on %s, so there is no fault to inject", volumeID, node.Name)
	}
	return pid
}

// signalNbdkit sends sig to pid, having re-confirmed the pid still belongs to
// the volume. The re-check is not paranoia: pids are reused, and the gap
// between resolving one and signalling it is exactly where a reused pid would
// land us on an unrelated process.
func signalNbdkit(ctx context.Context, f *Fixture, node harness.Node, volumeID string, pid int, sig string) error {
	current, err := nbdkitPIDForVolume(ctx, f.SSH, node, volumeID)
	if err != nil {
		return err
	}
	if current != pid {
		return fmt.Errorf("refusing to signal pid %d on %s: it no longer serves %s (now %d)",
			pid, node.Name, volumeID, current)
	}
	if _, err := f.SSH.Run(ctx, node, fmt.Sprintf("sudo kill -%s %d", sig, pid)); err != nil {
		return fmt.Errorf("kill -%s %d on %s: %w", sig, pid, node.Name, err)
	}
	return nil
}

// freezeNbdkit SIGSTOPs the volume's nbdkit and registers the thaw before the
// signal is sent, so an abort between the two still recovers. A frozen nbdkit
// holds its NBD socket open and answers nothing, which the guest sees as a
// stall rather than an error.
func freezeNbdkit(t *testing.T, f *Fixture, node harness.Node, volumeID string, pid int) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// Best effort by design: on the kill variant the process is already
		// gone, and a failed SIGCONT there is not a test failure.
		_, _ = f.SSH.Run(ctx, node, fmt.Sprintf("sudo kill -CONT %d 2>/dev/null || true", pid))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := signalNbdkit(ctx, f, node, volumeID, pid, "STOP"); err != nil {
		t.Fatalf("freeze nbdkit for %s: %v", volumeID, err)
	}
	t.Logf("froze nbdkit pid %d serving %s on %s", pid, volumeID, node.Name)
	time.Sleep(nbdkitSettle)
}

// thawNbdkit SIGCONTs the volume's nbdkit.
func thawNbdkit(t *testing.T, f *Fixture, node harness.Node, pid int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := f.SSH.Run(ctx, node, fmt.Sprintf("sudo kill -CONT %d", pid)); err != nil {
		t.Errorf("thaw nbdkit pid %d on %s: %v", pid, node.Name, err)
		return
	}
	t.Logf("thawed nbdkit pid %d on %s", pid, node.Name)
}

// killNbdkit SIGKILLs the volume's nbdkit. Nothing restarts it: viperblockd's
// recovery only re-adopts survivors after its own restart and never starts a
// process, so the volume stays unserved until the instance is torn down.
//
// The guest reaches EIO by a different route to a freeze. The socket closes,
// and QEMU's nbd driver is left at the default reconnect-delay of 0, so
// requests fail immediately rather than pausing for a reconnect.
func killNbdkit(t *testing.T, f *Fixture, node harness.Node, volumeID string, pid int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := signalNbdkit(ctx, f, node, volumeID, pid, "KILL"); err != nil {
		t.Fatalf("kill nbdkit for %s: %v", volumeID, err)
	}
	t.Logf("killed nbdkit pid %d serving %s on %s", pid, volumeID, node.Name)
	time.Sleep(nbdkitSettle)
}
