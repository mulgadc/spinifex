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

// qemuUser is the account the guests run as. The relaunched socket has to be
// writable by it, because that is who reconnects to it.
const qemuUser = "spinifex-daemon"

// nbdkitUser is the account viperblockd starts nbdkit as. The relaunch matches
// it so the process keeps the same access to the volume's cache and key.
const nbdkitUser = "spinifex-viperblock"

// nbdkitRelaunchTimeout bounds the wait for a relaunched nbdkit to rebind its
// socket. Opening a volume reads its state from the backend, so this is not
// instant.
const nbdkitRelaunchTimeout = 90 * time.Second

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

// nbdkitCmdline reads a running nbdkit's argv, so the process can be recreated
// exactly. Capturing it before the kill is what makes the relaunch faithful:
// the socket path carries a launch-time nonce that cannot be derived from the
// volume id.
func nbdkitCmdline(ctx context.Context, f *Fixture, node harness.Node, pid int) (string, error) {
	raw, err := f.SSH.Run(ctx, node, fmt.Sprintf("sudo tr '\\0' '\\n' < /proc/%d/cmdline", pid))
	if err != nil {
		return "", fmt.Errorf("read cmdline of pid %d on %s: %w", pid, node.Name, err)
	}
	args := strings.Fields(string(raw))
	if len(args) == 0 {
		return "", fmt.Errorf("pid %d on %s has an empty cmdline", pid, node.Name)
	}
	return strings.Join(args, " "), nil
}

// nbdkitSocketPath pulls the --unix argument out of a captured argv. The
// relaunch must bind the same path or QEMU has nothing to reconnect to.
func nbdkitSocketPath(cmdline string) (string, error) {
	args := strings.Fields(cmdline)
	for i, a := range args {
		if a == "--unix" && i+1 < len(args) {
			return args[i+1], nil
		}
	}
	return "", fmt.Errorf("no --unix argument in nbdkit cmdline %q", cmdline)
}

// killedNbdkit is what a kill leaves behind: enough to put the process back.
// envFile is a path on the node, never the credentials themselves — see
// captureNbdkitEnv.
type killedNbdkit struct {
	cmdline string
	socket  string
	envFile string
}

// captureNbdkitEnv copies the credential variables out of a running nbdkit's
// environment into a file on the same node. viperblockd passes them this way
// rather than on the command line so they stay out of the world-readable
// /proc/<pid>/cmdline, so a relaunch built from argv alone fails with
// "access_key parameter is required".
//
// The values are never read back into the test: the file is written on the
// node, consumed on the node and deleted there, so they stay exactly as
// confined as the original design intended.
func captureNbdkitEnv(ctx context.Context, f *Fixture, node harness.Node, volumeID string, pid int) (string, error) {
	path := fmt.Sprintf("/tmp/nbdkit-env-%s", volumeID)
	capture := fmt.Sprintf(
		"sudo sh -c 'umask 077; tr \"\\0\" \"\\n\" < /proc/%d/environ | grep -E \"^VB_(ACCESS|SECRET)_KEY=\" > %s' && "+
			"sudo chown %s %s && sudo test -s %s",
		pid, path, nbdkitUser, path, path)
	if _, err := f.SSH.Run(ctx, node, capture); err != nil {
		return "", fmt.Errorf("capture nbdkit credentials on %s: %w", node.Name, err)
	}
	return path, nil
}

// killNbdkit SIGKILLs the volume's nbdkit, first capturing what is needed to
// bring it back. Nothing restarts it on its own — viperblockd re-adopts
// survivors after its own restart and never starts a process — so the volume
// stays unserved until relaunchNbdkit puts it back.
//
// The guest reaches this by a different route to a freeze: the socket closes
// rather than going quiet, so requests fail instead of blocking.
func killNbdkit(t *testing.T, f *Fixture, node harness.Node, volumeID string, pid int) killedNbdkit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmdline, err := nbdkitCmdline(ctx, f, node, pid)
	if err != nil {
		t.Fatalf("capture nbdkit cmdline before killing it: %v", err)
	}
	socket, err := nbdkitSocketPath(cmdline)
	if err != nil {
		t.Fatalf("locate the nbd socket for %s: %v", volumeID, err)
	}
	envFile, err := captureNbdkitEnv(ctx, f, node, volumeID, pid)
	if err != nil {
		t.Fatalf("capture the environment before killing nbdkit for %s: %v", volumeID, err)
	}
	t.Cleanup(func() {
		cleanCtx, cancelClean := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelClean()
		_, _ = f.SSH.Run(cleanCtx, node, fmt.Sprintf("sudo rm -f %s", envFile))
	})

	if err := signalNbdkit(ctx, f, node, volumeID, pid, "KILL"); err != nil {
		t.Fatalf("kill nbdkit for %s: %v", volumeID, err)
	}
	t.Logf("killed nbdkit pid %d serving %s on %s (socket %s)", pid, volumeID, node.Name, socket)
	time.Sleep(nbdkitSettle)
	return killedNbdkit{cmdline: cmdline, socket: socket, envFile: envFile}
}

// reapRelaunchedNbdkit kills the relaunched server when the test ends. setsid
// detaches it from the SSH session that started it, and viperblockd never
// learned it exists, so nothing else will ever reap it.
//
// The PID is resolved from the socket path rather than the volume id: a
// legitimate viperblockd nbdkit for the same volume must not be killed, and
// only this one is bound to this path.
func reapRelaunchedNbdkit(t *testing.T, f *Fixture, node harness.Node, socket string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		out, err := f.SSH.Run(ctx, node,
			fmt.Sprintf("sudo pgrep -a nbdkit | grep -F -- %q | awk '{print $1}'", socket))
		if err != nil {
			t.Logf("could not look up relaunched nbdkit on %s: %v", node.Name, err)
			return
		}
		for _, pid := range strings.Fields(string(out)) {
			if _, err := f.SSH.Run(ctx, node, fmt.Sprintf("sudo kill %s", pid)); err != nil {
				t.Logf("could not stop relaunched nbdkit %s on %s: %v", pid, node.Name, err)
				continue
			}
			t.Logf("stopped relaunched nbdkit %s on %s", pid, node.Name)
		}
		if _, err := f.SSH.Run(ctx, node, fmt.Sprintf("sudo rm -f %s", socket)); err != nil {
			t.Logf("could not remove %s on %s: %v", socket, node.Name, err)
		}
	})
}

// relaunchNbdkit puts the volume's NBD server back on the same socket path,
// which is what turns the kill into a recoverable fault rather than a
// destructive one. The stale socket is removed first: nbdkit will not bind a
// path that already exists, and a SIGKILL leaves the file behind.
//
// It runs as the same user under setsid, because nbdkit was started with -f
// and would otherwise die with the SSH session that launched it.
func relaunchNbdkit(t *testing.T, f *Fixture, node harness.Node, volumeID string, k killedNbdkit) {
	t.Helper()
	// Budgeted to outlast the poll below with room for its round trips. Sharing
	// a shorter context with the poll left it expired by the time the
	// diagnostics ran, so a failure reported nothing at all.
	ctx, cancel := context.WithTimeout(context.Background(), nbdkitRelaunchTimeout+3*time.Minute)
	defer cancel()

	// Output goes to a file rather than /dev/null: an nbdkit that starts and
	// exits is the interesting failure, and its reason is only on stderr.
	// The credentials are sourced inside the target user's shell rather than
	// placed on the command line, so they are no more exposed than viperblockd
	// leaves them. Reading with `export "$line"` keeps any character in the
	// value intact, which quoting into a command string would not.
	log := fmt.Sprintf("/tmp/nbdkit-relaunch-%s.log", volumeID)
	start := fmt.Sprintf(
		"sudo rm -f %s; sudo touch %s && sudo chmod 666 %s && "+
			"sudo -u %s bash -c 'while IFS= read -r l; do export \"$l\"; done < %s; exec setsid %s' "+
			"</dev/null >%s 2>&1 & sleep 5; cat %s",
		k.socket, log, log, nbdkitUser, k.envFile, k.cmdline, log, log)
	// The early output is read here rather than only on failure: a later query
	// can fail on its own and take the reason with it.
	early, err := f.SSH.Run(ctx, node, start)
	if err != nil {
		t.Fatalf("relaunch nbdkit for %s on %s: %v", volumeID, node.Name, err)
	}
	if len(strings.TrimSpace(string(early))) > 0 {
		t.Logf("nbdkit relaunch said: %s", strings.TrimSpace(string(early)))
	}

	// QEMU reconnects to the path, not to the process, so the socket being
	// back and writable by the account QEMU runs as is the whole signal. The
	// chmod is viperblockd's own step, not a workaround: nbdkit binds the
	// socket 0755 and a QEMU with no group-write bit can never reconnect.
	ready := fmt.Sprintf("sudo test -S %s && sudo chmod 0770 %s && sudo -u %s test -w %s",
		k.socket, k.socket, qemuUser, k.socket)
	deadline := time.Now().Add(nbdkitRelaunchTimeout)
	for time.Now().Before(deadline) {
		if _, err := f.SSH.Run(ctx, node, ready); err == nil {
			t.Logf("relaunched nbdkit for %s on %s, socket %s is back", volumeID, node.Name, k.socket)
			reapRelaunchedNbdkit(t, f, node, k.socket)
			return
		}
		time.Sleep(2 * time.Second)
	}

	out, _ := f.SSH.Run(ctx, node, fmt.Sprintf(
		"cat %s 2>/dev/null; echo '--- socket ---'; sudo ls -la %s 2>&1; echo '--- procs ---'; pgrep -ax nbdkit | head",
		log, k.socket))
	t.Fatalf("nbdkit for %s did not rebind %s on %s within %s\ncommand: %s\n%s",
		volumeID, k.socket, node.Name, nbdkitRelaunchTimeout, k.cmdline, out)
}
