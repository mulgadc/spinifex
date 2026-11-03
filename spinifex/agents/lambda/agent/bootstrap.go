//go:build linux

package agent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sandboxID is the uid and gid the function's bootstrap runs as.
const sandboxID = 993

// StartBootstrap starts the function's bootstrap at path as the sandbox uid and
// gid with no supplementary groups, with no_new_privs, and with exactly env,
// in the bootstrap's own directory. The caller waits on the returned command.
func StartBootstrap(ctx context.Context, path string, env []string, stdout, stderr io.Writer) (*exec.Cmd, error) {
	cred := &syscall.Credential{Uid: sandboxID, Gid: sandboxID, Groups: []uint32{}}
	return startBootstrap(ctx, path, env, stdout, stderr, cred)
}

func startBootstrap(ctx context.Context, path string, env []string, stdout, stderr io.Writer, cred *syscall.Credential) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, path)
	// A nil Env inherits the agent's environment; the bootstrap gets only env.
	cmd.Env = append([]string{}, env...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Packaged functions open their files relative to the task root.
	cmd.Dir = filepath.Dir(path)
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	started := make(chan error, 1)
	go func() {
		// no_new_privs is per thread and the child inherits it from this one. The
		// thread is never unlocked, so Go ends it when this goroutine returns.
		goruntime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			started <- fmt.Errorf("set no_new_privs: %w", err)
			return
		}
		started <- cmd.Start()
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return cmd, nil
}
