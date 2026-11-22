//go:build e2e

package storagefault

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// volumeLeaseBucket is viperblockd's per-volume lease bucket. A launch epoch
// is a revision of the volume's entry, and every renewal raises it.
const volumeLeaseBucket = "VIPERBLOCK_VOLUME_LEASES"

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
	pid     int
	// leaseEpoch is the killed launch's epoch, 0 when it was given none.
	leaseEpoch uint64
	// lease is the volume's lease entry as read just before the kill.
	lease leaseSnapshot
}

// leaseEpochArgPrefix is the nbdkit argument carrying a launch's lease epoch.
const leaseEpochArgPrefix = "lease_epoch="

// leaseEpochWait bounds the wait for the lease entry to be renewed past the
// killed launch's epoch; viperblockd renews it every 10 s.
const leaseEpochWait = 60 * time.Second

// leaseEpochArg returns the lease_epoch argument of a captured nbdkit argv.
func leaseEpochArg(cmdline string) (uint64, bool) {
	for _, a := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(a, leaseEpochArgPrefix); ok {
			epoch, err := strconv.ParseUint(v, 10, 64)
			return epoch, err == nil
		}
	}
	return 0, false
}

// withLeaseEpoch replaces the lease_epoch argument in place, leaving every
// other argument as captured. An argv without one is returned unchanged.
func withLeaseEpoch(cmdline string, epoch uint64) string {
	args := strings.Fields(cmdline)
	for i, a := range args {
		if strings.HasPrefix(a, leaseEpochArgPrefix) {
			args[i] = leaseEpochArgPrefix + strconv.FormatUint(epoch, 10)
		}
	}
	return strings.Join(args, " ")
}

// leaseSnapshot is the holder and revision of a volume's lease entry.
type leaseSnapshot struct {
	Owner      string `json:"owner"`
	Generation uint64 `json:"generation"`
	revision   uint64
}

// leaseKV opens viperblockd's lease bucket over a harness NATS connection.
func leaseKV(ctx context.Context, t *testing.T, f *Fixture) jetstream.KeyValue {
	t.Helper()
	js, err := jetstream.New(harness.DialClusterNATS(t, f.Env))
	if err != nil {
		t.Fatalf("jetstream context: %v", err)
	}
	kv, err := js.KeyValue(ctx, volumeLeaseBucket)
	if err != nil {
		t.Fatalf("open %s: %v", volumeLeaseBucket, err)
	}
	return kv
}

func readLease(ctx context.Context, kv jetstream.KeyValue, volumeID string) (leaseSnapshot, error) {
	entry, err := kv.Get(ctx, volumeID)
	if err != nil {
		return leaseSnapshot{}, fmt.Errorf("read the lease entry for %s: %w", volumeID, err)
	}
	var snap leaseSnapshot
	if err := json.Unmarshal(entry.Value(), &snap); err != nil {
		return leaseSnapshot{}, fmt.Errorf("decode the lease entry for %s: %w", volumeID, err)
	}
	snap.revision = entry.Revision()
	return snap, nil
}

// requireLease reads the volume's lease entry or fails the test.
func requireLease(t *testing.T, f *Fixture, volumeID string) leaseSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snap, err := readLease(ctx, leaseKV(ctx, t, f), volumeID)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// requireLeaseUnchanged fails unless the entry still names the holder seen at
// the kill, so a replacement that outlived a lease loss cannot pass.
func requireLeaseUnchanged(t *testing.T, f *Fixture, volumeID string, atKill leaseSnapshot) {
	t.Helper()
	now := requireLease(t, f, volumeID)
	if now.Owner != atKill.Owner || now.Generation != atKill.Generation {
		t.Fatalf("lease for %s changed hands during the test: %s generation %d at the kill, %s generation %d now",
			volumeID, atKill.Owner, atKill.Generation, now.Owner, now.Generation)
	}
}

// freshLeaseEpoch waits for the holder seen at the kill to renew past both the
// killed epoch and the revision read at the kill, and returns that revision.
// It only reads, so viperblockd's compare-and-set renewals are untouched.
func freshLeaseEpoch(t *testing.T, f *Fixture, volumeID string, k killedNbdkit) (uint64, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), leaseEpochWait+30*time.Second)
	defer cancel()
	kv := leaseKV(ctx, t, f)
	floor := max(k.leaseEpoch, k.lease.revision)
	var last leaseSnapshot
	var first uint64
	deadline := time.Now().Add(leaseEpochWait)
	for time.Now().Before(deadline) {
		snap, err := readLease(ctx, kv, volumeID)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Owner != k.lease.Owner || snap.Generation != k.lease.Generation {
			t.Fatalf("lease for %s changed hands after the kill: %s generation %d, then %s generation %d",
				volumeID, k.lease.Owner, k.lease.Generation, snap.Owner, snap.Generation)
		}
		if first == 0 {
			first = snap.revision
		}
		// A rise seen here is a renewal within the last renewal period.
		if last = snap; snap.revision > floor && snap.revision > first {
			return snap.revision, time.Now()
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("lease entry for %s reached revision %d from %d, not above %d with a renewal seen, in %s",
		volumeID, last.revision, first, floor, leaseEpochWait)
	return 0, time.Time{}
}

// requireNbdkitGone fails unless the killed process has exited, so the
// replacement is never a second writer beside it.
func requireNbdkitGone(t *testing.T, f *Fixture, node harness.Node, pid int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.SSH.Run(ctx, node, fmt.Sprintf("sudo test ! -e /proc/%d", pid)); err != nil {
		t.Fatalf("killed nbdkit pid %d is still present on %s: %v", pid, node.Name, err)
	}
}

// relaunchLogPath is where the relaunched nbdkit's output goes on the node.
func relaunchLogPath(volumeID string) string {
	return fmt.Sprintf("/tmp/nbdkit-relaunch-%s.log", volumeID)
}

// logRelaunchOutput prints the relaunched nbdkit's output into a failed test.
func logRelaunchOutput(t *testing.T, f *Fixture, node harness.Node, volumeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := f.SSH.Run(ctx, node, "tail -n 80 "+relaunchLogPath(volumeID))
	t.Logf("relaunched nbdkit output on %s (err=%v):\n%s", node.Name, err, out)
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

	epoch, hasEpoch := leaseEpochArg(cmdline)
	var lease leaseSnapshot
	if hasEpoch {
		lease = requireLease(t, f, volumeID)
	}

	if err := signalNbdkit(ctx, f, node, volumeID, pid, "KILL"); err != nil {
		t.Fatalf("kill nbdkit for %s: %v", volumeID, err)
	}
	t.Logf("killed nbdkit pid %d serving %s on %s (socket %s)", pid, volumeID, node.Name, socket)
	time.Sleep(nbdkitSettle)
	return killedNbdkit{cmdline: cmdline, socket: socket, envFile: envFile, pid: pid, leaseEpoch: epoch, lease: lease}
}

// relaunchNbdkit puts the volume's NBD server back on the same socket path,
// which is what turns the kill into a recoverable fault rather than a
// destructive one. The stale socket is removed first: nbdkit will not bind a
// path that already exists, and a SIGKILL leaves the file behind.
//
// It runs as the same user under setsid, because nbdkit was started with -f
// and would otherwise die with the SSH session that launched it.
func relaunchNbdkit(t *testing.T, f *Fixture, node harness.Node, volumeID string, k killedNbdkit, r *replacement) {
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
	log := relaunchLogPath(volumeID)
	requireNbdkitGone(t, f, node, k.pid)
	cmdline := k.cmdline
	if k.leaseEpoch != 0 {
		// A new process may never reopen the killed launch's epoch.
		epoch, seenAt := freshLeaseEpoch(t, f, volumeID, k)
		cmdline = withLeaseEpoch(k.cmdline, epoch)
		t.Logf("relaunching %s at lease epoch %d, above the killed launch's %d", volumeID, epoch, k.leaseEpoch)
		r.guard(t, volumeID, k.lease, epoch, seenAt)
	}
	r.mu.Lock()
	r.spawned = true
	r.mu.Unlock()
	start := fmt.Sprintf(
		"sudo rm -f %s; sudo touch %s && sudo chmod 666 %s && "+
			"sudo -u %s bash -c 'while IFS= read -r l; do export \"$l\"; done < %s; exec setsid %s' "+
			"</dev/null >%s 2>&1 & sleep 5; cat %s",
		k.socket, log, log, nbdkitUser, k.envFile, cmdline, log, log)
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
			if found, err := r.resolve(ctx); !found {
				t.Fatalf("cannot identify the relaunched nbdkit on %s: found=%v err=%v", k.socket, found, err)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}

	out, _ := f.SSH.Run(ctx, node, fmt.Sprintf(
		"cat %s 2>/dev/null; echo '--- socket ---'; sudo ls -la %s 2>&1; echo '--- procs ---'; pgrep -ax nbdkit | head",
		log, k.socket))
	t.Fatalf("nbdkit for %s did not rebind %s on %s within %s\ncommand: %s\n%s",
		volumeID, k.socket, node.Name, nbdkitRelaunchTimeout, cmdline, out)
}
