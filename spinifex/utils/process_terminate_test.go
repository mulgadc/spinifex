package utils_test

import (
	"bufio"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// The unmount path calls this on an nbdkit that is already sealing, so the
// grace has to be spent only when the process needs it: one that exits on
// SIGTERM must be reaped in milliseconds, not after the full budget.
func TestTerminateProcessReturnsAsSoonAsTheProcessExits(t *testing.T) {
	cmd := exec.Command("sleep", "60") // dies immediately on default SIGTERM
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	start := time.Now()
	exited := utils.TerminateProcess(pid, 10*time.Second)
	elapsed := time.Since(start)

	assert.True(t, exited, "a process that dies on SIGTERM must be reported as exited")
	assert.Less(t, elapsed, time.Second, "must not wait out the grace period")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after TerminateProcess returned")
	}
}

// A plugin wedged mid-drain must not hold the unmount open past the grace:
// the caller escalates to ForceKillProcess, and can only do that if this
// returns.
func TestTerminateProcessGivesUpAtTheGraceAndDoesNotKill(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; echo ready; while true; do sleep 1; done")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "expected ready line from test process")
	require.Equal(t, "ready", scanner.Text())

	const grace = 200 * time.Millisecond
	start := time.Now()
	exited := utils.TerminateProcess(pid, grace)
	elapsed := time.Since(start)

	assert.False(t, exited)
	assert.GreaterOrEqual(t, elapsed, grace, "must wait out the grace before giving up")
	assert.Less(t, elapsed, grace+2*time.Second, "must be bounded by the grace, not hang")
	assert.True(t, utils.ProcessAlive(pid), "escalation belongs to the caller, not here")
}

func TestTerminateProcessAlreadyExitedIsSuccess(t *testing.T) {
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	assert.True(t, utils.TerminateProcess(pid, time.Second))
	assert.False(t, utils.TerminateProcess(0, time.Second), "an invalid pid is not an exit")
}
