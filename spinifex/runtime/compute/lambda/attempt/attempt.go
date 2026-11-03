// Package attempt runs one Lambda guest VM for one invocation inside the
// daemon: the daemon owns its deadline, and the kernel kills the VM if the
// daemon dies first.
package attempt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// StopBound is how long Run waits for a killed VM to be reaped before it
// reports the stop unproven.
const StopBound = 10 * time.Second

var (
	// ErrStopUnproven means there is no evidence the VM exited. Anything the
	// attempt holds must be kept until Outcome.Reaped closes, which may be never.
	ErrStopUnproven = errors.New("lambda attempt: VM stop not proven")
	// ErrTimedOut means the deadline passed before the session returned.
	ErrTimedOut = errors.New("lambda attempt: deadline passed")
	// ErrVMExited means the VM exited before the session returned.
	ErrVMExited = errors.New("lambda attempt: VM exited before the session returned")
	// ErrNoPidfd means the kernel gave no pidfd, so the VM is never signalled.
	ErrNoPidfd = errors.New("lambda attempt: no pidfd for the VM")
)

// waitVM waits for the VM; tests replace it to simulate a failed wait.
var waitVM = (*exec.Cmd).Wait

// pidfdSupported reports whether pidfds can be opened and signalled; tests
// replace it to simulate a kernel or seccomp policy without them.
var pidfdSupported = func() bool {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.PidfdSendSignal(fd, 0, nil, 0) == nil
}

// Outcome reports how the VM ended.
type Outcome struct {
	// TimedOut is true when the deadline passed before the attempt ended.
	TimedOut bool
	// Stopped is true only once the process is known to have exited.
	Stopped bool
	// Reaped closes when the process is known to have exited, possibly after
	// Run returns; it stays open if the wait failed.
	Reaped <-chan struct{}
}

// Run starts cmd, runs session while the VM lives, then kills and reaps the
// VM. It returns nil only when the session returned nil before the deadline,
// the VM exit and any caller cancel, and the VM was then proven stopped.
func Run(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, session func(context.Context) error) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{Stopped: true, Reaped: closedChan()}, err
	}
	if !pidfdSupported() {
		// Without a stable handle the VM could only be signalled by PID.
		return Outcome{Stopped: true, Reaped: closedChan()}, ErrNoPidfd
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// The parent-death signal follows the thread that forked the child, so the
	// launching goroutine stays locked to its thread until the VM is reaped.
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	// A pidfd taken at clone is the only handle signalled, so a kill can never
	// reach a reused PID, even if something else reaps the VM.
	pidfd := -1
	cmd.SysProcAttr.PidFD = &pidfd

	started := make(chan error, 1)
	reaped := make(chan struct{})
	waitFailed := make(chan error, 1)
	wait := func() {
		err := waitVM(cmd)
		if cmd.ProcessState == nil {
			waitFailed <- err
			return
		}
		close(reaped)
	}
	go func() {
		runtime.LockOSThread() // never unlocked: the thread ends with this goroutine
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		if pidfd < 0 {
			// Best effort, unproven: ending this thread delivers the parent-death
			// signal unless Go parks it as the main thread. Without exit evidence
			// the attempt is held.
			started <- ErrNoPidfd
			return
		}
		started <- nil
		wait()
	}()
	if err := <-started; errors.Is(err, ErrNoPidfd) {
		go wait()
		out, stopErr := awaitStop(Outcome{Reaped: reaped}, reaped, waitFailed)
		if stopErr != nil {
			return out, stopErr
		}
		return out, err
	} else if err != nil {
		// A failed Start leaves no child: os/exec reaps a fork whose exec failed.
		return Outcome{Stopped: true, Reaped: closedChan()}, fmt.Errorf("lambda attempt: start VM: %w", err)
	}
	defer func() { _ = unix.Close(pidfd) }()

	if err := ctx.Err(); err != nil {
		// Cancelled while the VM started: stop it before any session runs.
		return stop(Outcome{Reaped: reaped}, pidfd, reaped, waitFailed, err)
	}

	deadline := time.Now().Add(timeout)
	sessCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	sessDone := make(chan error, 1)
	go func() { sessDone <- session(sessCtx) }()

	out := Outcome{Reaped: reaped}
	var result error
	sessReturned := false
	select {
	case result = <-sessDone:
		sessReturned = true
	case <-reaped:
		result = ErrVMExited
	case err := <-waitFailed:
		// The VM may already be someone else's to reap: signal nothing.
		return out, fmt.Errorf("%w: wait: %w", ErrStopUnproven, err)
	case <-sessCtx.Done():
		result = ctx.Err()
	}
	// Classified by the monotonic clock and the caller's own context, not by
	// which case won, so a result racing the deadline or a cancel never succeeds.
	if !time.Now().Before(deadline) {
		out.TimedOut = true
		result = ErrTimedOut
	} else if err := ctx.Err(); err != nil {
		result = err
	}
	cancel()

	out, err := stop(out, pidfd, reaped, waitFailed, result)
	if errors.Is(err, ErrStopUnproven) {
		return out, err
	}
	if !sessReturned {
		select {
		case <-sessDone:
		case <-time.After(StopBound):
		}
	}
	return out, result
}

// stop kills the VM through its pidfd and returns result once it is proven
// stopped, or ErrStopUnproven.
func stop(out Outcome, pidfd int, reaped <-chan struct{}, waitFailed <-chan error, result error) (Outcome, error) {
	if err := unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return out, fmt.Errorf("%w: kill: %w", ErrStopUnproven, err)
	}
	out, err := awaitStop(out, reaped, waitFailed)
	if err != nil {
		return out, err
	}
	return out, result
}

// awaitStop waits up to StopBound for evidence that the VM exited.
func awaitStop(out Outcome, reaped <-chan struct{}, waitFailed <-chan error) (Outcome, error) {
	select {
	case <-reaped:
		out.Stopped = true
		return out, nil
	case err := <-waitFailed:
		return out, fmt.Errorf("%w: wait: %w", ErrStopUnproven, err)
	case <-time.After(StopBound):
		return out, ErrStopUnproven
	}
}

func closedChan() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}
