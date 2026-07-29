package testutil

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// SocketTempDir returns a data dir short enough to hold a unix socket path.
// t.TempDir() embeds the test name, which pushes the drain socket underneath it
// past the 108-byte sun_path limit for anything but the shortest test names.
func SocketTempDir(t *testing.T) string {
	t.Helper()

	//nolint:usetesting // t.TempDir() yields a path too long for the unix socket
	// sun_path (108-byte limit); MkdirTemp("") keeps it short under /tmp.
	dir, err := os.MkdirTemp("", "spx")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// StartDrainSocket stands in for the NBD plugin's drain socket, at the path the
// plugin serves it on the node hosting volumeID ({dataDir}/viperblock/{volumeID}
// /snapshot.sock). Every connection is answered with ack ("OK\n" for a drain
// that reached S3, "ERR\n" for one that did not) and closed.
func StartDrainSocket(t *testing.T, dataDir, volumeID, ack string) {
	t.Helper()
	serveDrainSocket(t, dataDir, volumeID, ack, nil, nil)
}

// StartBlockingDrainSocket stands in for a plugin whose flush is still running:
// it accepts but does not ack until release is called. accepted closes on the
// first connection, so a test can be sure the drain is in flight before
// asserting on what happens alongside it.
func StartBlockingDrainSocket(t *testing.T, dataDir, volumeID, ack string) (accepted <-chan struct{}, release func()) {
	t.Helper()

	acceptedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	serveDrainSocket(t, dataDir, volumeID, ack, acceptedCh, releaseCh)

	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(releaseCh) }) })

	return acceptedCh, func() { once.Do(func() { close(releaseCh) }) }
}

// serveDrainSocket answers every connection with ack, optionally closing
// accepted on the first one and waiting for release before it replies.
func serveDrainSocket(t *testing.T, dataDir, volumeID, ack string, accepted chan struct{}, release <-chan struct{}) {
	t.Helper()

	dir := filepath.Join(dataDir, "viperblock", volumeID)
	require.NoError(t, os.MkdirAll(dir, 0o750))

	ln, err := net.Listen("unix", filepath.Join(dir, "snapshot.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		var once sync.Once
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by the cleanup above
			}
			if accepted != nil {
				once.Do(func() { close(accepted) })
			}
			if release != nil {
				<-release
			}
			_, _ = conn.Write([]byte(ack))
			_ = conn.Close()
		}
	}()
}
