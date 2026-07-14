package utils

import (
	"bufio"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// KillProcess must poll for liveness instead of blindly sleeping, so a
// process that dies quickly (nbdkit on unmount, in production) is reaped
// in milliseconds rather than costing a fixed 1s+ per call.
func TestKillProcessReturnsQuicklyWhenProcessExitsPromptly(t *testing.T) {
	cmd := exec.Command("sleep", "60") // dies immediately on default SIGTERM
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	start := time.Now()
	err := KillProcess(pid)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 300*time.Millisecond,
		"a process that dies promptly must be reaped well under the old fixed 1s sleep")
	assert.False(t, ProcessAlive(pid))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after KillProcess returned")
	}
}

// The escalation-to-SIGKILL path must still be bounded, not hang forever,
// when a process ignores SIGTERM. Uses a shortened grace period so the test
// doesn't have to wait out the real (much longer) production grace period.
func TestKillProcessEscalatesToSigkillWithinDeadline(t *testing.T) {
	// Ignore SIGTERM so the process only dies via the SIGKILL escalation.
	// The loop (rather than a single "sleep") stops the shell from exec-ing
	// straight into sleep, which would silently drop the trap. Wait for the
	// "ready" line so we never signal before the trap is installed.
	cmd := exec.Command("sh", "-c", "trap '' TERM; echo ready; while true; do sleep 1; done")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "expected ready line from test process")
	require.Equal(t, "ready", scanner.Text())

	const pollInterval = 20 * time.Millisecond
	const gracePeriod = 300 * time.Millisecond

	start := time.Now()
	err = killProcessWithTiming(pid, pollInterval, gracePeriod)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, gracePeriod,
		"must wait out the full grace period before escalating")
	assert.Less(t, elapsed, gracePeriod+2*time.Second,
		"escalation must be bounded near the grace period, not hang indefinitely")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after KillProcess escalated to SIGKILL")
	}
	assert.False(t, ProcessAlive(pid), "process must be gone after SIGKILL escalation")
}
