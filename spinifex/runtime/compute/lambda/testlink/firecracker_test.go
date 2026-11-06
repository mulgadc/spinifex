//go:build lambdatestlink

//test:in-package drives the unexported supervisor, ack and pidfd helpers against real processes

package testlink

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/attempt"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// supervised starts this test binary as the Firecracker supervisor over a
// stand-in VMM that runs until killed, and returns the supervisor and the
// VMM's pidfd from the ack.
func supervised(t *testing.T, deadline time.Duration) (*exec.Cmd, int) {
	t.Helper()
	dir := t.TempDir()
	vmm := filepath.Join(dir, "firecracker")
	require.NoError(t, os.WriteFile(vmm, []byte("#!/bin/sh\nexec tail -f /dev/null\n"), 0o755))
	kernel, err := os.Open(os.DevNull)
	require.NoError(t, err)
	initrd, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = kernel.Close(); _ = initrd.Close() })
	d := &Daemon{Firecracker: vmm, Image: &Image{Kernel: kernel, Initrd: initrd}}
	ack, child, err := ackPair()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ack.Close() })
	cmd, err := d.supervisorCommand(filepath.Join(dir, fcConfigFile), deadline, child)
	require.NoError(t, err)
	cmd.Path = os.Args[0]
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	require.NoError(t, cmd.Start())
	_ = child.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	pidfd, pid, err := awaitFirecracker(context.Background(), ack, cmd.Process.Pid, armWait)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); _ = unix.Close(pidfd) })
	require.NotEqual(t, cmd.Process.Pid, pid)
	return cmd, pidfd
}

func boottime(t *testing.T) time.Duration {
	t.Helper()
	now, err := bootNow()
	require.NoError(t, err)
	return now
}

func TestFirecrackerSupervisorKillsAtBoottimeDeadlineWhileStopped(t *testing.T) {
	cmd, pidfd := supervised(t, boottime(t)+1500*time.Millisecond)

	// The kernel holds a SIGKILL timer on CLOCK_BOOTTIME (7), so the deadline counts suspend.
	timers, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/timers")
	require.NoError(t, err)
	assert.Contains(t, string(timers), "signal: 9/")
	assert.Contains(t, string(timers), "ClockID: 7")

	// A stopped supervisor still loses the VMM at the deadline: both kills come from the kernel.
	require.NoError(t, cmd.Process.Signal(syscall.SIGSTOP))
	exited, err := pidfdExited(pidfd, 5*time.Second)
	require.NoError(t, err)
	assert.True(t, exited, "VMM outlived its deadline")
	state, err := cmd.Process.Wait()
	require.NoError(t, err)
	assert.Equal(t, syscall.SIGKILL, state.Sys().(syscall.WaitStatus).Signal())
}

func TestFirecrackerDiesWithItsSupervisor(t *testing.T) {
	cmd, pidfd := supervised(t, boottime(t)+time.Hour)
	exited, err := pidfdExited(pidfd, 0)
	require.NoError(t, err)
	require.False(t, exited)

	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()
	// Only the kernel's parent-death signal can end the VMM here; nothing else signals it.
	exited, err = pidfdExited(pidfd, 5*time.Second)
	require.NoError(t, err)
	assert.True(t, exited, "VMM outlived its supervisor")
}

// A supervisor that never started launched nothing, so the attempt is
// released; one that started without handing over a pidfd may have started
// Firecracker, so the attempt is held.
func TestSettleFirecracker_OnlyANeverStartedSupervisorIsProvenStopped(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]struct {
		ctx  context.Context
		cmd  func() *exec.Cmd
		want bool
	}{
		"cancelled before Start": {cancelled, func() *exec.Cmd { return exec.Command("/bin/true") }, true},
		"failed Start":           {context.Background(), func() *exec.Cmd { return exec.Command(filepath.Join(t.TempDir(), "missing")) }, true},
		"started, no pidfd":      {context.Background(), func() *exec.Cmd { return exec.Command("/bin/true") }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := tc.cmd()
			fc := &vmHandle{fd: -1}
			out, _ := attempt.Run(tc.ctx, cmd, 10*time.Second, func(context.Context, int) error {
				fc.callbackDone()
				return errors.New("supervisor exited before its ack")
			})
			require.True(t, out.Stopped, "the supervisor itself is proven stopped")
			<-out.SessionDone
			assert.Equal(t, tc.want, settleFirecracker(out.SessionDone, fc, cmd.Process != nil))
		})
	}
}

func TestFirecrackerAckRequiresTheSupervisorsChild(t *testing.T) {
	ack, child, err := ackPair()
	require.NoError(t, err)
	defer func() { _ = ack.Close() }()
	self, err := unix.PidfdOpen(os.Getpid(), 0)
	require.NoError(t, err)
	require.NoError(t, unix.Sendmsg(int(child.Fd()), []byte(fcAck), unix.UnixRights(self), nil, 0))
	_ = unix.Close(self)
	_ = child.Close()

	_, _, err = awaitFirecracker(context.Background(), ack, 1, time.Second)
	require.ErrorIs(t, err, ErrNotArmed)
	assert.Contains(t, err.Error(), "not the supervisor's child")
}

// The archive the daemon writes is read by the guest's own identity reader
// once extracted, as the runtime image's init extracts it.
func TestConfigArchiveIsReadByTheGuestReaders(t *testing.T) {
	gen, err := guestconn.NewGeneration("ap-southeast-2/000000000001/fn/env/1", time.Hour)
	require.NoError(t, err)
	entries := map[string][]byte{fwCfgNetcfg: []byte("NIC0_MAC=02:00:00:00:00:01\n")}
	for name, b := range gen.GuestEntries("192.0.2.1:4000") {
		entries[lambdav1.FwCfgPrefix+name] = b
	}
	path := filepath.Join(t.TempDir(), configArchive)
	require.NoError(t, writeConfigArchive(path, entries))

	root := filepath.Join(t.TempDir(), "cfg")
	extract(t, path, root)
	_, err = hostlink.LoadIdentity(filepath.Join(root, "by_name", lambdav1.FwCfgPrefix))
	require.NoError(t, err)
	netcfg, err := os.ReadFile(filepath.Join(root, "by_name", fwCfgNetcfg, "raw"))
	require.NoError(t, err)
	assert.Equal(t, entries[fwCfgNetcfg], netcfg)
}

func extract(t *testing.T, archive, root string) {
	t.Helper()
	b, err := os.ReadFile(archive)
	require.NoError(t, err)
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		require.NoError(t, err)
		require.False(t, strings.Contains(h.Name, ".."), h.Name)
		p := filepath.Join(root, h.Name)
		if h.Typeflag == tar.TypeDir {
			require.NoError(t, os.MkdirAll(p, 0o700))
			continue
		}
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, data, 0o600))
	}
}
