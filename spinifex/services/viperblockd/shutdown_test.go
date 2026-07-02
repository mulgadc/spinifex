package viperblockd

import (
	"net"
	"os/exec"
	"path/filepath"
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

// TestNbdkitInUse_ConnectedClientDetected proves nbdkitInUse parses ss output
// and detects a live client on the unix socket — a guest still attached. The
// state column is field[1] (field[0] is the netid), so a wrong index here
// would silently report "idle" and let the SIGTERM path kill an in-use nbdkit.
func TestNbdkitInUse_ConnectedClientDetected(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss not available")
	}
	sock := filepath.Join(t.TempDir(), "nbd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	srvConn := <-accepted
	defer srvConn.Close()

	if !nbdkitInUse(MountedVolume{Name: "vol-attached", Socket: sock}) {
		t.Fatal("nbdkitInUse returned false for a socket with a connected client")
	}
}

// TestNbdkitInUse_NoSocketNotInUse: a path with no socket has no ESTAB row, so
// an idle nbdkit is reapable.
func TestNbdkitInUse_NoSocketNotInUse(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss not available")
	}
	sock := filepath.Join(t.TempDir(), "absent.sock")
	if nbdkitInUse(MountedVolume{Name: "vol-idle", Socket: sock}) {
		t.Fatal("nbdkitInUse returned true for a socket with no connections")
	}
}

// TestNbdkitInUse_TCPAssumedInUse: TCP transport can't be cheaply confirmed
// idle, so it defaults to in-use (safe — never kill under a guest).
func TestNbdkitInUse_TCPAssumedInUse(t *testing.T) {
	if !nbdkitInUse(MountedVolume{Name: "vol-tcp", Port: 10809}) {
		t.Fatal("nbdkitInUse must assume TCP transport is in use")
	}
}
