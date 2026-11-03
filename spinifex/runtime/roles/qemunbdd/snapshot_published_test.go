package qemunbdd_test

import (
	"bufio"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attachedClient stands in for a guest's QEMU: a qemu-io holding a connection
// to the export open, so the snapshot runs against a volume in use.
type attachedClient struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Scanner
}

func attachClient(t *testing.T, uri string) *attachedClient {
	t.Helper()
	cmd := exec.Command("qemu-io", "-f", "raw", uri)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = cmd.Stdout
	require.NoError(t, cmd.Start())
	c := &attachedClient{cmd: cmd, stdin: stdin, out: bufio.NewScanner(stdout)}
	t.Cleanup(c.detach)
	return c
}

// run sends one qemu-io command and returns the line reporting its result.
func (c *attachedClient) run(t *testing.T, command, want string) {
	t.Helper()
	_, err := io.WriteString(c.stdin, command+"\n")
	require.NoError(t, err)
	done := make(chan string, 1)
	go func() {
		for c.out.Scan() {
			if line := c.out.Text(); strings.Contains(line, want) || strings.Contains(line, "failed") {
				done <- line
				return
			}
		}
		done <- "qemu-io exited"
	}()
	select {
	case line := <-done:
		require.Contains(t, line, want)
	case <-time.After(10 * time.Second):
		t.Fatalf("qemu-io did not answer %q", command)
	}
}

func (c *attachedClient) detach() {
	_, _ = io.WriteString(c.stdin, "quit\n")
	_ = c.stdin.Close()
	_ = c.cmd.Wait()
}

// A snapshot of a volume a guest is attached to must succeed and hold what
// the guest wrote through the export, which is the only path a write takes.
func TestQEMUNBDSnapshotOfAttachedVolumeHoldsItsWrites(t *testing.T) {
	requireQEMUTools(t)
	provider := newQEMUProvider(t)
	ctx := t.Context()
	const volumeID, snapshotID = "vol-attached-snap", "snap-attached-snap"
	_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 64 << 20}})
	require.NoError(t, err)
	pub, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: "node-1"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: "node-1"})
	})

	guest := attachClient(t, pub.Export.NBD.NBDURI)
	guest.run(t, "write -P 0xab 1M 64k", "wrote 65536/65536")

	snap, err := provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volumeID})
	require.NoError(t, err)
	require.Equal(t, ebsprovider.SnapshotStateCompleted, snap.State)

	snapPath := strings.TrimPrefix(snap.Handle, "file://")
	out, err := exec.Command("qemu-io", "-f", "qcow2", "-r", "-c", "read -P 0xab 1M 64k", snapPath).CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Contains(t, string(out), "read 65536/65536")
	assert.NotContains(t, string(out), "Pattern verification failed")

	// The guest keeps its connection through the snapshot.
	guest.run(t, "read -P 0xab 1M 64k", "read 65536/65536")
}
