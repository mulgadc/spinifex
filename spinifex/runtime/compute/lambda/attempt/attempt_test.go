package attempt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The test binary doubles as the fake VM and as a fake daemon, chosen by env.
const helperEnv = "SPX_ATTEMPT_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "hang":
		select {}
	case "exit":
		os.Exit(0)
	case "daemon":
		os.Exit(fakeDaemon())
	}
	os.Exit(m.Run())
}

func helperCmd(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	return cmd
}

// fakeDaemon runs one attempt whose VM never exits and prints the VM's PID.
func fakeDaemon() int {
	vm := helperCmd("hang")
	_, _ = Run(context.Background(), vm, time.Hour, func(ctx context.Context) error {
		fmt.Println(vm.Process.Pid)
		<-ctx.Done()
		return nil
	})
	return 0
}

// pidfdOpen takes a handle on pid while its parent is known to be alive and
// not reaping it, so the PID cannot have been reused yet.
func pidfdOpen(t *testing.T, pid int) int {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
	})
	return fd
}

// exitedWithin reports whether the process behind fd exits within d; a pidfd
// becomes readable when its process exits.
func exitedWithin(t *testing.T, fd int, d time.Duration) bool {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(d.Milliseconds()))
	require.NoError(t, err)
	return n == 1
}

func TestRun_DeadlineKillsAndReapsTheVM(t *testing.T) {
	vm := helperCmd("hang")
	sawDeadline := false
	out, err := Run(t.Context(), vm, 200*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		sawDeadline = errors.Is(ctx.Err(), context.DeadlineExceeded)
		return nil
	})
	require.ErrorIs(t, err, ErrTimedOut, "a session returning nil at the deadline is not a result")
	assert.True(t, out.TimedOut)
	assert.True(t, out.Stopped)
	assert.True(t, sawDeadline, "the session's context ends at the deadline")
	require.NotNil(t, vm.ProcessState, "the VM was reaped")
}

func TestRun_SessionEndStopsTheVM(t *testing.T) {
	vm := helperCmd("hang")
	out, err := Run(t.Context(), vm, time.Minute, func(context.Context) error { return nil })
	require.NoError(t, err)
	assert.False(t, out.TimedOut)
	assert.True(t, out.Stopped)
	require.NotNil(t, vm.ProcessState)
}

func TestRun_SessionErrorIsReturnedAfterTheStop(t *testing.T) {
	boom := errors.New("guest refused")
	vm := helperCmd("hang")
	out, err := Run(t.Context(), vm, time.Minute, func(context.Context) error { return boom })
	require.ErrorIs(t, err, boom)
	assert.True(t, out.Stopped)
	require.NotNil(t, vm.ProcessState)
}

func TestRun_VMExitBeforeAResultIsAFailure(t *testing.T) {
	vm := helperCmd("exit")
	out, err := Run(t.Context(), vm, time.Minute, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	require.ErrorIs(t, err, ErrVMExited, "a session returning nil once cancelled is not a result")
	assert.False(t, out.TimedOut)
	assert.True(t, out.Stopped)
}

func TestRun_CallerCancelIsNotATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	vm := helperCmd("hang")
	out, err := Run(ctx, vm, time.Minute, func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, out.TimedOut)
	assert.True(t, out.Stopped)
}

func TestRun_StartFailureNeverRunsTheSession(t *testing.T) {
	called := false
	out, err := Run(t.Context(), exec.Command("/nonexistent/qemu"), time.Minute, func(context.Context) error {
		called = true
		return nil
	})
	require.Error(t, err)
	assert.True(t, out.Stopped, "a failed start leaves no process")
	assert.False(t, called)
}

func TestRun_SetsTheParentDeathSignal(t *testing.T) {
	vm := helperCmd("hang")
	vm.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_, err := Run(t.Context(), vm, time.Minute, func(context.Context) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, syscall.SIGKILL, vm.SysProcAttr.Pdeathsig)
	require.NotNil(t, vm.SysProcAttr.PidFD, "the VM is signalled only through its pidfd")
	assert.True(t, vm.SysProcAttr.Setpgid, "caller attributes are kept")
}

// startFakeDaemon starts a fake daemon process and returns it with a handle on its VM.
func startFakeDaemon(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	daemon := helperCmd("daemon")
	stdout, err := daemon.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, daemon.Start())
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	})
	lines := bufio.NewScanner(stdout)
	require.True(t, lines.Scan(), "fake daemon printed no VM PID")
	pid, err := strconv.Atoi(lines.Text())
	require.NoError(t, err)
	fd := pidfdOpen(t, pid)
	require.False(t, exitedWithin(t, fd, 0), "the VM runs while the daemon lives")
	return daemon, fd
}

func TestRun_DaemonDeathKillsTheVM(t *testing.T) {
	daemon, vm := startFakeDaemon(t)
	require.NoError(t, daemon.Process.Signal(syscall.SIGKILL))
	_ = daemon.Wait()
	assert.True(t, exitedWithin(t, vm, 5*time.Second), "the kernel killed the VM with its daemon")
}

// A result handed over after the deadline, before the deadline's timer has
// run, is still a timeout. One P makes the session hold it past the deadline.
func TestRun_ResultAfterTheDeadlineIsATimeout(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	vm := helperCmd("hang")
	out, err := Run(t.Context(), vm, time.Millisecond, func(context.Context) error {
		for start := time.Now(); time.Since(start) < 5*time.Millisecond; {
		}
		return nil
	})
	require.ErrorIs(t, err, ErrTimedOut)
	assert.True(t, out.TimedOut)
	assert.True(t, out.Stopped)
}

func TestRun_FailedWaitLeavesTheStopUnproven(t *testing.T) {
	waitVM = func(*exec.Cmd) error { return errors.New("wait failed") }
	t.Cleanup(func() { waitVM = (*exec.Cmd).Wait })
	vm := helperCmd("hang")
	out, err := Run(t.Context(), vm, time.Minute, func(context.Context) error { return nil })
	require.ErrorIs(t, err, ErrStopUnproven)
	assert.False(t, out.Stopped)
	select {
	case <-out.Reaped:
		t.Fatal("Reaped closed without exit evidence")
	default:
	}
	_ = vm.Wait()
}

// cancelOnly is a caller context whose cancellation reaches derived contexts
// only through a goroutine, so a session result can win the race against it.
type cancelOnly struct {
	context.Context

	done chan struct{}
}

func (c *cancelOnly) Done() <-chan struct{} { return c.done }

func (c *cancelOnly) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// A nil result handed over after the caller cancelled is not a success, even
// when Run sees the result before the cancellation reaches the session.
func TestRun_ResultRacingACallerCancelIsNotASuccess(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	ctx := &cancelOnly{Context: t.Context(), done: make(chan struct{})}
	vm := helperCmd("hang")
	out, err := Run(ctx, vm, time.Minute, func(context.Context) error {
		close(ctx.done)
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, out.TimedOut)
	assert.True(t, out.Stopped)
}

func TestRun_RefusesToLaunchWithoutPidfds(t *testing.T) {
	orig := pidfdSupported
	pidfdSupported = func() bool { return false }
	t.Cleanup(func() { pidfdSupported = orig })
	called := false
	vm := helperCmd("hang")
	out, err := Run(t.Context(), vm, time.Minute, func(context.Context) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, ErrNoPidfd)
	assert.True(t, out.Stopped)
	assert.Nil(t, vm.Process, "QEMU was never started")
	assert.False(t, called)
}

func TestRun_CancelledBeforeLaunchStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	vm := helperCmd("hang")
	out, err := Run(ctx, vm, time.Minute, func(context.Context) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, out.Stopped)
	assert.Nil(t, vm.Process, "QEMU was never started")
	assert.False(t, called)
}

// cancelOnStart cancels the caller as soon as the VM has been created.
type cancelOnStart struct {
	context.Context

	cancel func()
	calls  int
}

func (c *cancelOnStart) Err() error {
	c.calls++
	if c.calls == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRun_CancelledDuringStartStopsTheVMWithoutASession(t *testing.T) {
	inner, cancel := context.WithCancel(t.Context())
	ctx := &cancelOnStart{Context: inner, cancel: cancel}
	called := false
	vm := helperCmd("hang")
	out, err := Run(ctx, vm, time.Minute, func(context.Context) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, out.Stopped)
	require.NotNil(t, vm.ProcessState, "the started VM was stopped and reaped")
	assert.False(t, called)
}
