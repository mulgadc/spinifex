package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadPID is above the default pid_max, so it names no process.
const deadPID = 1 << 30

// shutdownPhaseReply sends payload to handler over NATS and returns the ACK it
// answered with and the outcome it reported.
func shutdownPhaseReply(t *testing.T, handler func(*nats.Msg) string, payload []byte) (ShutdownACK, string) {
	t.Helper()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	outcome := make(chan string, 1)
	subject := nats.NewInbox()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) { outcome <- handler(msg) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	reply, err := nc.Request(subject, payload, 5*time.Second)
	require.NoError(t, err)

	var ack ShutdownACK
	require.NoError(t, json.Unmarshal(reply.Data, &ack))
	return ack, <-outcome
}

// stubNBDKitProcs makes every pid look like a live nbdkit and records which
// were signalled, so a STORAGE phase can never reach a real process.
func stubNBDKitProcs(t *testing.T, signalErr error) *[]int {
	t.Helper()
	origComm, origSignal := procComm, signalProcess
	t.Cleanup(func() { procComm, signalProcess = origComm, origSignal })

	var signalled []int
	procComm = func(int) (string, error) { return "nbdkit", nil }
	signalProcess = func(pid int, _ syscall.Signal) error {
		signalled = append(signalled, pid)
		return signalErr
	}
	return &signalled
}

// shutdownTestDaemon is the plain daemon the phase handlers need: a node name
// and a config whose pid directory is private to the test.
func shutdownTestDaemon(t *testing.T, services ...string) (*Daemon, string) {
	t.Helper()
	d := &Daemon{node: "node-1", config: &config.Config{Services: services}}
	return d, configurePidDir(t, d)
}

func TestShutdownMalformedPayloads(t *testing.T) {
	d := &Daemon{node: "node-1", config: &config.Config{}}

	for phase, handler := range map[string]func(*nats.Msg) string{
		"storage": d.handleShutdownStorage,
		"persist": d.handleShutdownPersist,
		"infra":   d.handleShutdownInfra,
	} {
		t.Run(phase, func(t *testing.T) {
			ack, outcome := shutdownPhaseReply(t, handler, []byte("{"))
			assert.Equal(t, outcomeError, outcome)
			assert.Equal(t, phase, ack.Phase)
			assert.Equal(t, "node-1", ack.Node)
			assert.NotEmpty(t, ack.Error, "the coordinator has to be told why the phase did not run")
			assert.Empty(t, ack.Stopped)
		})
	}

	t.Run("an ACK that cannot be sent is logged", func(t *testing.T) {
		logs := captureSlogForTest(t)
		assert.Equal(t, outcomeError, d.handleShutdownPersist(noReplyMsg("shutdown.persist", []byte("{"))))
		assert.Contains(t, logs.String(), "Failed to respond with shutdown ACK")
	})
}

func TestHandleShutdownStorage(t *testing.T) {
	t.Run("a viperblock already gone is not reported stopped", func(t *testing.T) {
		runtimeDir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
		signalled := stubNBDKitProcs(t, nil)
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, "nbdkit-vol-a.pid"), []byte("4242"), 0o600))

		d, pidDir := shutdownTestDaemon(t, "viperblock")
		require.NoError(t, utils.WritePidFileTo(pidDir, "viperblock", deadPID))

		ack, outcome := shutdownPhaseReply(t, d.handleShutdownStorage, []byte(`{"phase":"storage"}`))
		assert.Equal(t, outcomeSuccess, outcome)
		assert.Equal(t, "storage", ack.Phase)
		assert.Empty(t, ack.Error)
		assert.Empty(t, ack.Stopped, "a stop that failed must not be reported as one")
		assert.NoFileExists(t, filepath.Join(pidDir, "viperblock.pid"), "the stale pid file is cleared")
		assert.Equal(t, []int{4242}, *signalled, "this node's nbdkit backends are swept from its runtime dir")
	})

	t.Run("an unconfigured viperblock is left alone", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		signalled := stubNBDKitProcs(t, nil)

		d, pidDir := shutdownTestDaemon(t, "daemon")
		require.NoError(t, utils.WritePidFileTo(pidDir, "viperblock", deadPID))

		ack, outcome := shutdownPhaseReply(t, d.handleShutdownStorage, []byte(`{"phase":"storage"}`))
		assert.Equal(t, outcomeSuccess, outcome)
		assert.Empty(t, ack.Stopped)
		assert.FileExists(t, filepath.Join(pidDir, "viperblock.pid"))
		assert.Empty(t, *signalled)
	})
}

func TestHandleShutdownPersist(t *testing.T) {
	t.Run("a predastore already gone is not reported stopped", func(t *testing.T) {
		d, pidDir := shutdownTestDaemon(t, "predastore")
		require.NoError(t, utils.WritePidFileTo(pidDir, "predastore", deadPID))

		ack, outcome := shutdownPhaseReply(t, d.handleShutdownPersist, []byte(`{"phase":"persist"}`))
		assert.Equal(t, outcomeSuccess, outcome)
		assert.Equal(t, "persist", ack.Phase)
		assert.Empty(t, ack.Error)
		assert.Empty(t, ack.Stopped)
		assert.NoFileExists(t, filepath.Join(pidDir, "predastore.pid"))
	})

	t.Run("an unconfigured predastore is left alone", func(t *testing.T) {
		d, pidDir := shutdownTestDaemon(t, "daemon")
		require.NoError(t, utils.WritePidFileTo(pidDir, "predastore", deadPID))

		ack, outcome := shutdownPhaseReply(t, d.handleShutdownPersist, []byte(`{"phase":"persist"}`))
		assert.Equal(t, outcomeSuccess, outcome)
		assert.Empty(t, ack.Stopped)
		assert.FileExists(t, filepath.Join(pidDir, "predastore.pid"))
	})
}

// A service whose process is already gone still lets GATE complete: the daemon
// stops taking work either way, and only real stops are reported.
func TestHandleShutdownGate_DeadServices(t *testing.T) {
	d, pidDir := shutdownTestDaemon(t, "ui", "vpcd")
	require.NoError(t, utils.WritePidFileTo(pidDir, "spinifex-ui", deadPID))
	require.NoError(t, utils.WritePidFileTo(pidDir, "vpcd", deadPID))

	ack, outcome := shutdownPhaseReply(t, d.handleShutdownGate, []byte(`{"phase":"gate"}`))
	assert.Equal(t, outcomeSuccess, outcome)
	assert.Empty(t, ack.Stopped)
	assert.True(t, d.shuttingDown.Load())
}

// Neither the marker nor the local state write gates the ACK: by DRAIN the
// guests are already stopped, and refusing to advance would only strand them.
func TestHandleShutdownDrain_MarkerAndStateErrors(t *testing.T) {
	logs := captureSlogForTest(t)
	d := createTestDaemon(t, sharedNATSURL)
	d.jsManager = &JetStreamManager{}

	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	d.config.DataDir = filepath.Join(blocker, "data")

	ack, outcome := shutdownPhaseReply(t, d.handleShutdownDrain, []byte(`{"phase":"drain"}`))
	assert.Equal(t, outcomeSuccess, outcome)
	assert.Equal(t, "drain", ack.Phase)
	assert.Empty(t, ack.Error)
	assert.Contains(t, logs.String(), "Failed to write shutdown marker during DRAIN")
	assert.Contains(t, logs.String(), "Failed to write state during DRAIN")
}

func TestCleanupOrphanNBDKit_Failures(t *testing.T) {
	t.Run("an unlistable runtime dir signals nothing", func(t *testing.T) {
		logs := captureSlogForTest(t)
		signalled := stubNBDKitProcs(t, nil)

		cleanupOrphanNBDKit(filepath.Join(t.TempDir(), "["))
		assert.Empty(t, *signalled)
		assert.Contains(t, logs.String(), "cannot list pid files")
	})

	t.Run("a backend that cannot be signalled keeps its pid file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nbdkit-vol-stuck.pid")
		require.NoError(t, os.WriteFile(path, []byte("5151"), 0o600))
		signalled := stubNBDKitProcs(t, errors.New("operation not permitted"))

		cleanupOrphanNBDKit(dir)
		assert.Equal(t, []int{5151}, *signalled)
		assert.FileExists(t, path, "the backend may still be running, so its pid file is the only handle left on it")
	})

	t.Run("an unreadable pid file that cannot be removed is skipped", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nbdkit-vol-dir.pid")
		require.NoError(t, os.MkdirAll(filepath.Join(path, "child"), 0o750))
		signalled := stubNBDKitProcs(t, nil)

		cleanupOrphanNBDKit(dir)
		assert.Empty(t, *signalled)
		assert.DirExists(t, path)
	})
}

func TestPublishShutdownProgress_ClosedConn(t *testing.T) {
	logs := captureSlogForTest(t)
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	nc.Close()

	(&Daemon{node: "node-1", natsConn: nc}).publishShutdownProgress("drain", 2, 1)
	assert.Contains(t, logs.String(), "Failed to publish shutdown progress")
}

func TestPidDirEmpty(t *testing.T) {
	assert.Empty(t, (&Daemon{config: &config.Config{}}).pidDir(), "no base dir means no pid dir to look in")
	assert.Equal(t, "/data/logs", (&Daemon{config: &config.Config{BaseDir: "/data/spinifex/"}}).pidDir())
}
