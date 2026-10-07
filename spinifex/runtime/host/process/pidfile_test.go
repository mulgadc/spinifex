package process

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeneratePidFile(t *testing.T) {
	// Simulate a sample process running (e.g cat)
	cmd := exec.Command("cat")
	cmd.Start()

	err := WritePidFile("utilsunittest", cmd.Process.Pid)

	assert.NoError(t, err)

	// Read the PID file and verify contents
	pid, err := ReadPidFile("utilsunittest")

	assert.NoError(t, err)
	assert.Equal(t, cmd.Process.Pid, pid)

	// Test attempt to read a PID file that doesn't exist
	_, err = ReadPidFile("nonexistentpidfile")
	assert.Error(t, err)

	// Cleanup
	err = RemovePidFile("utilsunittest")
	assert.NoError(t, err)

	// Give some time before killing the process
	//time.Sleep(2 * time.Second)

	// Simulate process ending
}

func TestExecProcessAndKill(t *testing.T) {
	// Simulate a sample process running (e.g sleep, 30 secs)
	cmd := exec.Command("sleep", "30")

	// Detach: new process group, no controlling terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // put child in new process group
	}

	// Make it fully background-friendly:
	// - close stdio so parent doesn't block on pipes
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Start (non-blocking). If command is missing, error.
	if err := cmd.Start(); err != nil {
		assert.Fail(t, "Failed to start command", err)
	}

	// IMPORTANT: reap the child to avoid zombies.
	// Since we're "backgrounding" it, do Wait() in a goroutine.
	go func(c *exec.Cmd) {
		t.Log("Waiting for command to finish...")
		_ = c.Wait() // ignore error; ensures kernel reaps the process
		t.Log("Command finished.")
	}(cmd)

	err := WritePidFile("utilsunittest", cmd.Process.Pid)

	t.Log("Started process with PID:", cmd.Process.Pid)

	assert.NoError(t, err)

	// Test PID file removed
	err = WaitForPidFileRemoval("utilsunittest", 100*time.Millisecond)
	assert.Error(t, err) // Should timeout since file should still exist

	time.Sleep(20 * time.Millisecond)

	// Kill the process
	err = StopProcessAt("", "utilsunittest")
	assert.NoError(t, err)

	// Test PID file removed
	err = WaitForPidFileRemoval("utilsunittest", 1*time.Second)
	assert.NoError(t, err) // Should timeout since file should still exist

	// Verify process is killed
	err = cmd.Process.Signal(syscall.Signal(0))
	assert.Error(t, err) // Should return an error since process is killed
}

func TestWaitForPidFile(t *testing.T) {
	t.Parallel()
	t.Run("returns pid when file appears within timeout", func(t *testing.T) {
		t.Parallel()
		const name = "wait-pidfile-appears"
		_ = RemovePidFile(name)
		t.Cleanup(func() { _ = RemovePidFile(name) })

		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = WritePidFile(name, 4242)
		}()

		pid, err := WaitForPidFile(name, time.Second)
		require.NoError(t, err)
		assert.Equal(t, 4242, pid)
	})

	t.Run("returns error when timeout expires", func(t *testing.T) {
		t.Parallel()
		const name = "wait-pidfile-missing"
		_ = RemovePidFile(name)

		_, err := WaitForPidFile(name, 100*time.Millisecond)
		assert.Error(t, err)
	})

	t.Run("returns immediately when file already present", func(t *testing.T) {
		t.Parallel()
		const name = "wait-pidfile-present"
		require.NoError(t, WritePidFile(name, 7777))
		t.Cleanup(func() { _ = RemovePidFile(name) })

		start := time.Now()
		pid, err := WaitForPidFile(name, time.Second)
		require.NoError(t, err)
		assert.Equal(t, 7777, pid)
		assert.Less(t, time.Since(start), 50*time.Millisecond)
	})
}

func TestWaitForPidFileRemoval_AlreadyGoneReturnsBeforeFirstPoll(t *testing.T) {
	t.Parallel()
	const name = "wait-pidfile-removal-absent"
	_ = RemovePidFile(name)

	// The poll ticks every 100ms, so this timeout fails unless absence is seen up front.
	require.NoError(t, WaitForPidFileRemoval(name, 20*time.Millisecond))
}

func TestWaitForUnixSocket(t *testing.T) {
	t.Parallel()
	t.Run("returns nil when socket appears within timeout", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "appears.sock")

		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		require.NoError(t, WaitForUnixSocket(path, time.Second))
	})

	t.Run("returns error when timeout expires", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.sock")

		err := WaitForUnixSocket(path, 100*time.Millisecond)
		require.Error(t, err)
	})

	t.Run("waits for socket created after a delay", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "delayed.sock")

		go func() {
			time.Sleep(150 * time.Millisecond)
			ln, err := net.Listen("unix", path)
			if err == nil {
				t.Cleanup(func() { _ = ln.Close() })
			}
		}()

		require.NoError(t, WaitForUnixSocket(path, time.Second))
	})
}

func TestKillProcess(t *testing.T) {
	// Create a test process
	cmd := exec.Command("sleep", "60")
	err := cmd.Start()
	require.NoError(t, err)

	pid := cmd.Process.Pid

	// Reap in background so KillProcess can detect termination
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = cmd.Wait()
	})

	err = KillProcess(pid)
	assert.NoError(t, err)
	wg.Wait()

	// Test killing non-existent process
	err = KillProcess(999999)
	assert.Error(t, err, "Should error when killing non-existent process")
}

func TestProcessAlive(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	var wg sync.WaitGroup
	wg.Go(func() { _ = cmd.Wait() })

	assert.True(t, ProcessAlive(pid), "a running process must report alive")
	assert.False(t, ProcessAlive(0), "pid 0 must report not alive")
	assert.False(t, ProcessAlive(-1), "negative pid must report not alive")
	assert.False(t, ProcessAlive(999999), "a non-existent pid must report not alive")

	require.NoError(t, cmd.Process.Kill())
	wg.Wait()
	assert.False(t, ProcessAlive(pid), "a killed process must report not alive")
}

func TestForceKillProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	var wg sync.WaitGroup
	wg.Go(func() { _ = cmd.Wait() })

	require.NoError(t, ForceKillProcess(pid, 5*time.Second))
	wg.Wait()
	assert.False(t, ProcessAlive(pid), "ForceKillProcess must SIGKILL and confirm exit")

	assert.Error(t, ForceKillProcess(0, time.Second), "invalid pid must error")
}

// TestForceKillProcess_AlreadyExitedIsSuccess pins the contract callers rely on.
// This is asked "make sure this process is gone", and one that died on its own
// is as gone as one it killed — reporting a failure there makes a caller treat
// a stopped writer as a live one.
func TestForceKillProcess_AlreadyExitedIsSuccess(t *testing.T) {
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	assert.NoError(t, ForceKillProcess(pid, time.Second),
		"an already-exited process satisfies the request, it does not fail it")
}

func TestGeneratePidFile_EmptyName(t *testing.T) {
	_, err := GeneratePidFile("")
	assert.Error(t, err)
}

func TestWritePidFileTo(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("cat")
	require.NoError(t, cmd.Start())
	defer cmd.Process.Kill()

	// Write PID to custom directory
	err := WritePidFileTo(dir, "testservice", cmd.Process.Pid)
	require.NoError(t, err)

	// Read it back from the same directory
	pid, err := ReadPidFileFrom(dir, "testservice")
	require.NoError(t, err)
	assert.Equal(t, cmd.Process.Pid, pid)

	// Clean up
	err = RemovePidFileAt(dir, "testservice")
	assert.NoError(t, err)

	// Verify it's gone
	_, err = ReadPidFileFrom(dir, "testservice")
	assert.Error(t, err)
}

func TestWritePidFileTo_EmptyDir(t *testing.T) {
	// With empty dir, should fall back to default RuntimeDir()
	cmd := exec.Command("cat")
	require.NoError(t, cmd.Start())
	defer cmd.Process.Kill()

	err := WritePidFileTo("", "pidto-fallback", cmd.Process.Pid)
	require.NoError(t, err)

	// Should be readable via the default ReadPidFile
	pid, err := ReadPidFile("pidto-fallback")
	require.NoError(t, err)
	assert.Equal(t, cmd.Process.Pid, pid)

	// Clean up
	RemovePidFile("pidto-fallback")
}

func TestStopProcessAt(t *testing.T) {
	dir := t.TempDir()

	// Start a process we can kill
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())

	// Write PID file
	err := WritePidFileTo(dir, "stopat-test", cmd.Process.Pid)
	require.NoError(t, err)

	// Reap in background so StopProcessAt can detect termination
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = cmd.Wait()
	})

	err = StopProcessAt(dir, "stopat-test")
	assert.NoError(t, err)
	wg.Wait()

	// Verify PID file was removed
	_, err = ReadPidFileFrom(dir, "stopat-test")
	assert.Error(t, err, "PID file should be removed")
}

func TestStopProcessAt_StaleProcess(t *testing.T) {
	dir := t.TempDir()

	// Start a process and let it exit, leaving a stale PID file
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	stalePid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	// Write stale PID file
	err := WritePidFileTo(dir, "stale-test", stalePid)
	require.NoError(t, err)

	// StopProcessAt should return an error (process is dead) but still
	// clean up the PID file
	err = StopProcessAt(dir, "stale-test")
	assert.Error(t, err, "should error because process is already dead")

	// PID file must be removed despite the kill error
	_, err = ReadPidFileFrom(dir, "stale-test")
	assert.Error(t, err, "PID file should be removed even when process is already dead")
}

func TestStopProcessAt_NoPidFile(t *testing.T) {
	dir := t.TempDir()
	err := StopProcessAt(dir, "nonexistent")
	assert.Error(t, err, "should error when PID file does not exist")
}

func TestSetOOMScore(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OOM score adjustment only supported on Linux")
	}

	// Read current value so we can set something higher (unprivileged processes
	// can only increase their OOM score, not decrease it)
	pid := os.Getpid()
	current, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
	if err != nil {
		t.Skipf("Cannot read OOM score: %v", err)
	}

	// Set a positive score (always allowed for unprivileged processes)
	err = SetOOMScore(pid, 100)
	if err != nil {
		t.Skipf("Insufficient permissions to set OOM score: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
	assert.NoError(t, err)
	assert.Equal(t, "100", strings.TrimSpace(string(data)))

	// Best-effort restore (may fail without privileges if original was lower)
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), current, 0644)
}

func TestSetOOMScore_InvalidPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OOM score adjustment only supported on Linux")
	}

	err := SetOOMScore(999999999, 100)
	assert.Error(t, err)
}

func TestRuntimeDir(t *testing.T) {
	dir := RuntimeDir()
	assert.NotEmpty(t, dir)
}

func TestPidPath_XDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/test-xdg-runtime")
	assert.Equal(t, "/tmp/test-xdg-runtime", RuntimeDir())
}

func TestPidPath_HomeSpinifexFallback(t *testing.T) {
	tmpHome := t.TempDir()
	spinifexDir := fmt.Sprintf("%s/spinifex", tmpHome)
	require.NoError(t, os.Mkdir(spinifexDir, 0755))

	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", tmpHome)

	assert.Equal(t, spinifexDir, RuntimeDir())
}

func TestPidPath_TempDirFallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", "/nonexistent-home-dir-utils-test")

	assert.Equal(t, os.TempDir(), RuntimeDir())
}

func TestWritePidFile_CreateError(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent-dir-utils-test")
	err := WritePidFile("testservice", 12345)
	assert.Error(t, err)
}

func TestRemovePidFileAt_EmptyDir(t *testing.T) {
	err := RemovePidFileAt("", fmt.Sprintf("nonexistent-service-%d", time.Now().UnixNano()))
	assert.Error(t, err)
}

func TestGeneratePidFile_InvalidPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent-dir-utils-test")
	path, err := GeneratePidFile("test")
	// GeneratePidFile doesn't check if path exists, just builds it
	assert.NoError(t, err)
	assert.Contains(t, path, "test.pid")
}

func TestReadPidFileFrom_EmptyDir(t *testing.T) {
	_, err := ReadPidFileFrom("", fmt.Sprintf("nonexistent-service-%d", time.Now().UnixNano()))
	assert.Error(t, err)
}

func TestWaitForProcessExit_ProcessAlreadyDead(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	// `true` has exited and been reaped. A timeout shorter than one poll fails
	// unless WaitForProcessExit checks the PID before its first tick.
	err := WaitForProcessExit(pid, time.Duration(processExitPollNanos.Load())/2)
	assert.NoError(t, err)
}

func TestWaitForProcessExit_ProcessExitsBeforeTimeout(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "0.1")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	// Reap in the background so the kernel releases the PID once sleep exits.
	go func() { _ = cmd.Wait() }()

	err := WaitForProcessExit(pid, 5*time.Second)
	assert.NoError(t, err)
}

func TestWaitForProcessExit_Timeout(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	err := WaitForProcessExit(cmd.Process.Pid, 200*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}
