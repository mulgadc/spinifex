package viperblockd

import (
	"os/exec"
	"syscall"
	"testing"
)

// alive reports whether pid is still a live process.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestShutdownVolumes_InUseNotKilled asserts the SIGTERM handler leaves an
// nbdkit serving an attached guest running — killing it would corrupt the guest.
func TestShutdownVolumes_InUseNotKilled(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill() }()

	shutdownVolumes([]MountedVolume{{Name: "vol-inuse", PID: pid}}, func(MountedVolume) bool { return true })

	if !alive(pid) {
		t.Fatal("in-use nbdkit was killed on SIGTERM — guest corruption risk")
	}
}

// TestShutdownVolumes_IdleKilled asserts an nbdkit with no attached guest is
// reaped on SIGTERM, so a clean stop leaves no orphan.
func TestShutdownVolumes_IdleKilled(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill() }()

	// Reap concurrently: KillProcess SIGTERMs then polls Signal(0); without a
	// Wait the exited child lingers as a zombie and the poll runs full timeout.
	go func() { _, _ = cmd.Process.Wait() }()

	shutdownVolumes([]MountedVolume{{Name: "vol-idle", PID: pid}}, func(MountedVolume) bool { return false })

	if alive(pid) {
		t.Fatal("idle nbdkit was not reaped on SIGTERM")
	}
}
