package viperblockd

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// acceptingSocket stands in for nbdkit: something bound to a socket and
// accepting connections on it.
func acceptingSocket(t *testing.T) string {
	t.Helper()
	// Socket paths are capped near 108 bytes, so this cannot use t.TempDir().
	path := filepath.Join(t.TempDir(), "s")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return path
}

// TestWaitForNBDDial_ReturnsAsSoonAsItCanConnect is the point of the change.
// The check it replaced slept a fixed second and then asked only whether the
// process was still alive, so every mount paid a second nbdkit did not need.
func TestWaitForNBDDial_ReturnsAsSoonAsItCanConnect(t *testing.T) {
	path := acceptingSocket(t)

	start := time.Now()
	err := waitForNBDDial(t.Context(), "unix", path, make(chan int), time.Now().Add(nbdkitReadyDeadline))
	require.NoError(t, err)
	require.Less(t, time.Since(start), 200*time.Millisecond,
		"waiting for a listener that was already up took as long as the fixed delay it replaced")
}

// TestWaitForNBDDial_FailsWhenTheProcessExits keeps the fast-failure behaviour
// the old loop did have. A nbdkit that dies during startup must fail the mount
// straight away rather than waiting out the deadline.
func TestWaitForNBDDial_FailsWhenTheProcessExits(t *testing.T) {
	exitChan := make(chan int, 1)
	exitChan <- 3

	start := time.Now()
	err := waitForNBDDial(t.Context(), "unix", filepath.Join(t.TempDir(), "absent"), exitChan, time.Now().Add(time.Minute))
	require.ErrorContains(t, err, "code=3")
	require.Less(t, time.Since(start), time.Second, "an exited nbdkit was waited on")
}

// TestWaitForNBDDial_FailsOnDeadline covers the nbdkit that neither serves nor
// exits, which would otherwise hang the mount forever.
func TestWaitForNBDDial_FailsOnDeadline(t *testing.T) {
	err := waitForNBDDial(t.Context(), "unix", filepath.Join(t.TempDir(), "absent"), make(chan int), time.Now().Add(50*time.Millisecond))
	require.ErrorContains(t, err, "did not become ready")
}

// TestWaitForSocketFile_WaitsForCreation covers the ordering the unix path
// depends on: the socket has to exist before it can be chmod'd, and the chmod
// has to happen before a client running as another user can reach it.
func TestWaitForSocketFile_WaitsForCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "late.sock")

	go func() {
		time.Sleep(30 * time.Millisecond)
		listener, err := net.Listen("unix", path)
		if err == nil {
			t.Cleanup(func() { _ = listener.Close() })
		}
	}()

	require.NoError(t, waitForSocketFile(t.Context(), path, make(chan int), time.Now().Add(5*time.Second)))
}

// TestPollUntil_HonoursContextCancellation stops a mount whose caller has
// already given up from holding a goroutine until the deadline.
func TestPollUntil_HonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := pollUntil(ctx, func() error { return errors.New("never ready") },
		make(chan int), time.Now().Add(time.Minute), time.Sleep)
	require.ErrorIs(t, err, context.Canceled)
}
