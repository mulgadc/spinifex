//go:build lambdatestlink

package testlink

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
)

const recoverAttempt = "0123456789ab"

// recoverGrants answers only what recovery asks of the owner.
type recoverGrants struct {
	Grants
	cur                 execution.Attempt
	curErr              error
	releaseErr, stopErr error
	released, stopped   int
}

func (g *recoverGrants) Released(context.Context, execution.Grant) error {
	g.released++
	if g.releaseErr == nil {
		g.cur.State = execution.StateReleased
	}
	return g.releaseErr
}

func (g *recoverGrants) Current(context.Context, string) (execution.Attempt, error) {
	return g.cur, g.curErr
}

func (g *recoverGrants) Stopped(context.Context, execution.Grant) error {
	g.stopped++
	if g.stopErr == nil {
		g.cur.State = execution.StateEnded
	}
	return g.stopErr
}

// child starts a long sleep and returns it with its pidfs inode.
func child(t *testing.T) (*exec.Cmd, uint64) {
	t.Helper()
	fd := -1
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{PidFD: &fd}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			_ = cmd.Wait()
		}
		_ = unix.Close(fd)
	})
	ino, err := pidfsInode(fd)
	require.NoError(t, err)
	return cmd, ino
}

// alive reports whether cmd's process is still running, without reaping it.
func alive(cmd *exec.Cmd) bool {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	return err == nil && info.Signo == 0
}

// goneLauncher returns the PID and inode of a process that has exited.
func goneLauncher(t *testing.T) (int, uint64) {
	t.Helper()
	cmd, ino := child(t)
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	return cmd.Process.Pid, ino
}

func recordedLaunch(t *testing.T, vm *exec.Cmd, ino uint64) execution.Launch {
	t.Helper()
	h, err := readHost()
	require.NoError(t, err)
	lpid, lino := goneLauncher(t)
	return execution.Launch{MachineID: h.machine, BootID: h.boot, PIDNamespace: h.pidns,
		LauncherPID: lpid, LauncherInode: lino, PID: vm.Process.Pid, Inode: ino,
		TapName: "lm" + recoverAttempt, TapIndex: 7, TapOwner: "lambda-" + recoverAttempt}
}

type recoverRig struct {
	d      *Daemon
	grants *recoverGrants
	ips    *fakeIPs
	taps   *fakeTaps
	marker string
}

func newRecoverRig(t *testing.T, state string, l *execution.Launch) *recoverRig {
	t.Helper()
	work := t.TempDir()
	marker := filepath.Join(work, recoverAttempt+markerSuffix)
	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(work, recoverAttempt), 0o700))
	g := &recoverGrants{cur: execution.Attempt{State: state, Launch: l,
		Grant: execution.Grant{GrantID: "g", AttemptID: recoverAttempt, Node: testNode}}}
	ips, taps := &fakeIPs{}, &fakeTaps{}
	return &recoverRig{d: &Daemon{Node: testNode, WorkDir: work, Grants: g, IPs: ips, Taps: taps},
		grants: g, ips: ips, taps: taps, marker: marker}
}

func (r *recoverRig) assertHeld(t *testing.T) {
	t.Helper()
	assert.FileExists(t, r.marker)
	assert.DirExists(t, filepath.Join(r.d.WorkDir, recoverAttempt))
	assert.Empty(t, r.ips.released)
	assert.Empty(t, r.taps.removed)
	assert.Zero(t, r.grants.released)
	assert.Zero(t, r.grants.stopped)
}

func (r *recoverRig) assertRecovered(t *testing.T) {
	t.Helper()
	assert.NoFileExists(t, r.marker)
	assert.NoDirExists(t, filepath.Join(r.d.WorkDir, recoverAttempt))
	assert.Equal(t, []string{"lambda-" + recoverAttempt}, r.ips.released)
	assert.Equal(t, []host.OwnedTap{{Name: "lm" + recoverAttempt, IfIndex: 7, Owner: "lambda-" + recoverAttempt}}, r.taps.removed)
	assert.Equal(t, 1, r.grants.released)
	assert.Equal(t, 1, r.grants.stopped)
	assert.Equal(t, execution.StateEnded, r.grants.cur.State)
}

// Recovery proves nothing it cannot scope to this boot and PID namespace with
// no launcher left, and then it signals nothing and releases nothing.
func TestRecover_HoldsWhatItCannotProve(t *testing.T) {
	cases := map[string]func(l *execution.Launch, vm *exec.Cmd){
		"no launch identity": nil,
		"another boot":       func(l *execution.Launch, _ *exec.Cmd) { l.BootID = "other-boot" },
		"another host":       func(l *execution.Launch, _ *exec.Cmd) { l.MachineID = "other-host" },
		"another namespace":  func(l *execution.Launch, _ *exec.Cmd) { l.PIDNamespace++ },
		"launcher still runs": func(l *execution.Launch, _ *exec.Cmd) {
			l.LauncherPID = os.Getpid()
			ino, err := selfInode()
			if err == nil {
				l.LauncherInode = ino
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			vm, ino := child(t)
			var l *execution.Launch
			if change != nil {
				rec := recordedLaunch(t, vm, ino)
				change(&rec, vm)
				l = &rec
			}
			r := newRecoverRig(t, execution.StateRunning, l)
			r.d.Recover(context.Background())
			r.assertHeld(t)
			assert.True(t, alive(vm), "recovery signalled the process")
		})
	}
}

// A recorded VM whose PID has no process, or names a process with another
// pidfs inode, is gone: nothing is signalled and the attempt is released.
func TestRecover_ProvesAGoneVMWithoutSignalling(t *testing.T) {
	t.Run("no process", func(t *testing.T) {
		vm, ino := child(t)
		l := recordedLaunch(t, vm, ino)
		require.NoError(t, vm.Process.Kill())
		_ = vm.Wait()
		r := newRecoverRig(t, execution.StateAccepted, &l)
		r.d.Recover(context.Background())
		r.assertRecovered(t)
	})
	t.Run("PID now another process", func(t *testing.T) {
		vm, ino := child(t)
		l := recordedLaunch(t, vm, ino)
		l.Inode = ino + 1
		r := newRecoverRig(t, execution.StateRunning, &l)
		r.d.Recover(context.Background())
		r.assertRecovered(t)
		assert.True(t, alive(vm), "recovery signalled a process that was not the VM")
	})
}

// The recorded VM still running is killed through its own pidfd, and the
// attempt released only once that pidfd reports the exit.
func TestRecover_KillsTheRecordedVMBeforeReleasing(t *testing.T) {
	vm, ino := child(t)
	l := recordedLaunch(t, vm, ino)
	r := newRecoverRig(t, execution.StateAmbiguous, &l)
	r.d.Recover(context.Background())
	r.assertRecovered(t)
	assert.False(t, alive(vm))
	err := vm.Wait()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, syscall.SIGKILL, exit.Sys().(syscall.WaitStatus).Signal())
}

// Each step waits on the one before, and a later recovery finishes from
// whichever side of a failed step the attempt was left on.
func TestRecover_PartialProgressIsRetried(t *testing.T) {
	vm, ino := child(t)
	l := recordedLaunch(t, vm, ino)
	require.NoError(t, vm.Process.Kill())
	_ = vm.Wait()

	r := newRecoverRig(t, execution.StateRunning, &l)
	r.ips.releaseErr = errors.New("kv unavailable")
	r.d.Recover(context.Background())
	assert.FileExists(t, r.marker)
	assert.Zero(t, r.grants.released, "release recorded with the address unreleased")

	r.ips.releaseErr = nil
	r.grants.releaseErr = execution.ErrOutcomeUnknown
	r.d.Recover(context.Background())
	assert.FileExists(t, r.marker, "marker removed without a confirmed release")
	assert.Zero(t, r.grants.stopped)

	r.grants.releaseErr = nil
	r.grants.stopErr = execution.ErrOutcomeUnknown
	r.d.Recover(context.Background())
	assert.NoFileExists(t, r.marker)
	assert.Equal(t, execution.StateReleased, r.grants.cur.State, "node freed without a confirmed stop")

	// Released with the marker already gone: recovery only frees the node.
	r.grants.stopErr = nil
	released := r.grants.released
	r.d.Recover(context.Background())
	assert.Equal(t, execution.StateEnded, r.grants.cur.State)
	assert.Equal(t, released, r.grants.released)
}

// The launch identity comes from the exact clone-time pidfd, and an exited VM
// is refused before anything is recorded.
func TestLaunchIdentity(t *testing.T) {
	fd := -1
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{PidFD: &fd}
	require.NoError(t, cmd.Start())
	defer func() { _ = unix.Close(fd) }()

	l, err := launchIdentity(fd, cmd.Process.Pid, host.OwnedTap{Name: "lmx", IfIndex: 3, Owner: "lambda-x"})
	require.NoError(t, err)
	want, err := pidfsInode(fd)
	require.NoError(t, err)
	assert.Equal(t, want, l.Inode)
	assert.Equal(t, cmd.Process.Pid, l.PID)
	assert.Equal(t, os.Getpid(), l.LauncherPID)
	assert.NotEqual(t, l.Inode, l.LauncherInode)
	assert.Equal(t, "lmx", l.TapName)

	require.NoError(t, unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0))
	exited, err := pidfdExited(fd, 5*time.Second)
	require.NoError(t, err)
	require.True(t, exited)
	_, err = launchIdentity(fd, cmd.Process.Pid, host.OwnedTap{})
	assert.ErrorIs(t, err, ErrVMGone)
	_ = cmd.Wait()
}

// An invalid descriptor is never read as an exit.
func TestPidfdExited_InvalidDescriptorIsNotExit(t *testing.T) {
	exited, err := pidfdExited(1<<20, 0)
	assert.Error(t, err)
	assert.False(t, exited)
}

// A transient failure is retried until recovery converges.
func TestRecoverLoop_RetriesATransientFailure(t *testing.T) {
	vm, ino := child(t)
	l := recordedLaunch(t, vm, ino)
	require.NoError(t, vm.Process.Kill())
	_ = vm.Wait()
	r := newRecoverRig(t, execution.StateRunning, &l)
	r.ips.releaseFails = 2

	r.d.RecoverLoop(context.Background(), 5, time.Millisecond)
	assert.Equal(t, execution.StateEnded, r.grants.cur.State)
	assert.NoFileExists(t, r.marker)
	assert.Len(t, r.ips.released, 3)
	assert.False(t, r.d.busy.Load(), "recovery left the node slot taken")
}

// A round never runs while a request holds the node slot.
func TestRecoverLoop_NeverRunsBesideARequest(t *testing.T) {
	vm, ino := child(t)
	l := recordedLaunch(t, vm, ino)
	require.NoError(t, vm.Process.Kill())
	_ = vm.Wait()
	r := newRecoverRig(t, execution.StateRunning, &l)
	r.d.busy.Store(true)

	r.d.RecoverLoop(context.Background(), 3, time.Millisecond)
	r.assertHeld(t)
}
