package utils

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	commandOutputLimit = 64 * 1024
	commandWaitDelay   = time.Second
)

// RunCommandWithTimeout runs a host command with bounded output and execution.
// The command gets its own process group so timeout cleanup also kills child
// hooks rather than leaving them running after their parent exits.
func RunCommandWithTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = commandWaitDelay

	var output limitedCommandOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%s: %w", commandName(name, args), err)
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var err error
	select {
	case err = <-waitDone:
		// ErrWaitDelay means a descendant retained the output pipes after the
		// direct process exited. Kill its process group before returning.
		if errors.Is(err, exec.ErrWaitDelay) {
			_ = killProcessGroup(cmd)
		}
	case <-timer.C:
		if killErr := killProcessGroup(cmd); killErr != nil {
			_ = cmd.Process.Kill()
		}
		<-waitDone
		return commandOutputError(output.String(), fmt.Sprintf("%s timed out after %s", commandName(name, args), timeout), context.DeadlineExceeded)
	}

	if err != nil {
		return commandOutputError(output.String(), commandName(name, args), err)
	}
	return output.String(), nil
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func commandName(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

func commandOutputError(output, operation string, err error) (string, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed != "" {
		return output, fmt.Errorf("%s: %w: %s", operation, err, trimmed)
	}
	return output, fmt.Errorf("%s: %w", operation, err)
}

// limitedCommandOutput prevents a noisy resolver hook from growing the admin
// process without bound while retaining enough output for diagnostics.
type limitedCommandOutput struct {
	bytes.Buffer

	truncated bool
}

func (b *limitedCommandOutput) Write(p []byte) (int, error) {
	originalLen := len(p)
	remaining := commandOutputLimit - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(len(p), remaining)])
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return originalLen, nil
}

func (b *limitedCommandOutput) String() string {
	output := b.Buffer.String()
	if b.truncated {
		return output + "\n[command output truncated]"
	}
	return output
}
