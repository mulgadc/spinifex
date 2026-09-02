package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noReplyWait bounds a request that is expected to go unanswered. The handler
// drops a misaddressed message synchronously, so over loopback this only has to
// outlast a round trip, not the production reply budget.
const noReplyWait = 100 * time.Millisecond

// TestClusterShutdownStateKVRoundTrip verifies cluster shutdown state can be stored and retrieved from KV.
func TestClusterShutdownStateKVRoundTrip(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitClusterStateBucket()
	require.NoError(t, err)

	state := &ClusterShutdownState{
		Initiator:  "node1",
		Phase:      "drain",
		Started:    "2025-01-01T00:00:00Z",
		Timeout:    "2m0s",
		Force:      false,
		NodesTotal: 3,
		NodesAcked: map[string]string{
			"node1": "gate",
			"node2": "gate",
		},
	}

	err = jsm.WriteClusterShutdown(state)
	require.NoError(t, err)

	loaded, err := jsm.ReadClusterShutdown()
	require.NoError(t, err)
	require.NotNil(t, loaded)

	assert.Equal(t, state.Initiator, loaded.Initiator)
	assert.Equal(t, state.Phase, loaded.Phase)
	assert.Equal(t, state.Started, loaded.Started)
	assert.Equal(t, state.NodesTotal, loaded.NodesTotal)
	assert.Equal(t, state.Force, loaded.Force)
	assert.Len(t, loaded.NodesAcked, 2)

	// Cleanup
	err = jsm.DeleteClusterShutdown()
	require.NoError(t, err)
}

// TestRespondShutdownACK verifies respondShutdownACK marshals and sends the ACK via NATS request/reply.
func TestRespondShutdownACK(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	d := &Daemon{node: "test-node", natsConn: nc}

	tests := []struct {
		name string
		ack  ShutdownACK
	}{
		{
			name: "gate phase with stopped services",
			ack: ShutdownACK{
				Node:    "test-node",
				Phase:   "gate",
				Stopped: []string{"awsgw", "spinifex-ui"},
			},
		},
		{
			name: "drain phase with error",
			ack: ShutdownACK{
				Node:  "test-node",
				Phase: "drain",
				Error: "failed to stop VMs",
			},
		},
		{
			name: "storage phase empty stopped list",
			ack: ShutdownACK{
				Node:  "test-node",
				Phase: "storage",
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject := fmt.Sprintf("test.shutdown.ack.%d", i)

			// Subscribe and set up a handler that receives requests
			sub, err := nc.SubscribeSync(subject)
			require.NoError(t, err)
			defer sub.Unsubscribe()
			require.NoError(t, nc.Flush())

			// Send a NATS request — the handler will call msg.Respond()
			inbox := nc.NewRespInbox()
			replySub, err := nc.SubscribeSync(inbox)
			require.NoError(t, err)
			defer replySub.Unsubscribe()
			require.NoError(t, nc.Flush())

			err = nc.PublishRequest(subject, inbox, []byte("{}"))
			require.NoError(t, err)

			// Receive the request message and pass it to respondShutdownACK
			msg, err := sub.NextMsg(2 * time.Second)
			require.NoError(t, err)

			d.respondShutdownACK(msg, tt.ack)

			// Read the reply
			reply, err := replySub.NextMsg(2 * time.Second)
			require.NoError(t, err)

			var decoded ShutdownACK
			err = json.Unmarshal(reply.Data, &decoded)
			require.NoError(t, err)

			assert.Equal(t, tt.ack.Node, decoded.Node)
			assert.Equal(t, tt.ack.Phase, decoded.Phase)
			assert.Equal(t, tt.ack.Stopped, decoded.Stopped)
			assert.Equal(t, tt.ack.Error, decoded.Error)
		})
	}
}

// TestPublishShutdownProgress verifies publishShutdownProgress publishes correct progress to the NATS topic.
func TestPublishShutdownProgress(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	d := &Daemon{node: "progress-node", natsConn: nc}

	tests := []struct {
		name      string
		phase     string
		total     int
		remaining int
	}{
		{"initial drain progress", "drain", 5, 5},
		{"partial drain progress", "drain", 5, 2},
		{"final drain progress", "drain", 5, 0},
		{"zero VMs", "drain", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := nc.SubscribeSync("spinifex.cluster.shutdown.progress")
			require.NoError(t, err)
			defer sub.Unsubscribe()
			require.NoError(t, nc.Flush())

			d.publishShutdownProgress(tt.phase, tt.total, tt.remaining)
			require.NoError(t, nc.Flush())

			msg, err := sub.NextMsg(2 * time.Second)
			require.NoError(t, err)

			var progress ShutdownProgress
			err = json.Unmarshal(msg.Data, &progress)
			require.NoError(t, err)

			assert.Equal(t, "progress-node", progress.Node)
			assert.Equal(t, tt.phase, progress.Phase)
			assert.Equal(t, tt.total, progress.Total)
			assert.Equal(t, tt.remaining, progress.Remaining)
		})
	}
}

// configurePidDir overrides BaseDir so pidDir() resolves to an isolated tmp
// directory and returns that directory after creating it.
func configurePidDir(t *testing.T, d *Daemon) string {
	t.Helper()
	root := t.TempDir()
	d.config.BaseDir = filepath.Join(root, "spinifex")
	pidDir := filepath.Join(root, "logs")
	require.NoError(t, os.MkdirAll(pidDir, 0o750))
	return pidDir
}

// startSleepProcess forks a sleep process and arranges for it to be reaped
// when the test completes. Returns the PID.
func startSleepProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = cmd.Wait()
	})
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		wg.Wait()
	})
	return cmd.Process.Pid
}

// TestHandleShutdownGate covers the GATE phase handler: service stop fan-out,
// shuttingDown flag, and ACK reply for valid, malformed, and partial-failure inputs.
func TestHandleShutdownGate(t *testing.T) {
	t.Run("valid request stops configured services and sets shuttingDown", func(t *testing.T) {
		daemon := createTestDaemon(t, sharedNATSURL)
		daemon.config.Services = []string{"awsgw", "ui", "vpcd"}
		pidDir := configurePidDir(t, daemon)

		pid := startSleepProcess(t)
		require.NoError(t, utils.WritePidFileTo(pidDir, "awsgw", pid))

		subject := "spinifex.cluster.shutdown.gate"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownGate))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "gate"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 30*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))

		assert.Equal(t, "node-1", ack.Node)
		assert.Equal(t, "gate", ack.Phase)
		assert.Empty(t, ack.Error)
		assert.Contains(t, ack.Stopped, "awsgw")
		assert.True(t, daemon.shuttingDown.Load())

		_, statErr := os.Stat(filepath.Join(pidDir, "awsgw.pid"))
		assert.True(t, os.IsNotExist(statErr), "awsgw pid file should be removed")
	})

	t.Run("malformed json returns error ack", func(t *testing.T) {
		daemon := createTestDaemon(t, sharedNATSURL)
		daemon.config.Services = []string{}
		configurePidDir(t, daemon)

		subject := "spinifex.cluster.shutdown.gate.malformed"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownGate))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		reply, err := daemon.natsConn.Request(subject, []byte(`{not valid json}`), 5*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))

		assert.Equal(t, "gate", ack.Phase)
		assert.NotEmpty(t, ack.Error)
		assert.Empty(t, ack.Stopped)
		assert.False(t, daemon.shuttingDown.Load(), "shuttingDown must not be set on parse failure")
	})

	t.Run("partial service-stop failure still sends ack", func(t *testing.T) {
		daemon := createTestDaemon(t, sharedNATSURL)
		daemon.config.Services = []string{"awsgw", "ui", "vpcd"}
		pidDir := configurePidDir(t, daemon)

		pid := startSleepProcess(t)
		require.NoError(t, utils.WritePidFileTo(pidDir, "awsgw", pid))

		subject := "spinifex.cluster.shutdown.gate.partial"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownGate))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "gate"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 30*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))

		assert.Equal(t, "gate", ack.Phase)
		assert.Empty(t, ack.Error)
		assert.Equal(t, []string{"awsgw"}, ack.Stopped, "only the service with a live pid file should appear in stopped")
		assert.True(t, daemon.shuttingDown.Load())
	})
}

// TestShutdownRequestTarget covers the node scoping that keeps one node's
// local drain off every other node: the phase subjects are fan-out, so a
// request naming another node must be dropped before any service is stopped.
func TestShutdownRequestTarget(t *testing.T) {
	t.Run("request for another node is ignored with no ack", func(t *testing.T) {
		daemon := createTestDaemon(t, sharedNATSURL)
		daemon.config.Services = []string{"awsgw", "ui", "vpcd"}
		pidDir := configurePidDir(t, daemon)

		pid := startSleepProcess(t)
		require.NoError(t, utils.WritePidFileTo(pidDir, "awsgw", pid))

		subject := "spinifex.cluster.shutdown.gate.targeted-elsewhere"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownGate))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "gate", Target: "some-other-node"})
		require.NoError(t, err)

		_, err = daemon.natsConn.Request(subject, payload, noReplyWait)
		require.Error(t, err, "a request for another node must not be answered")

		assert.False(t, daemon.shuttingDown.Load(), "another node's drain must not gate this daemon")
		_, statErr := os.Stat(filepath.Join(pidDir, "awsgw.pid"))
		assert.NoError(t, statErr, "another node's drain must not stop this node's services")
	})

	t.Run("request for this node is honoured", func(t *testing.T) {
		daemon := createTestDaemon(t, sharedNATSURL)
		daemon.config.Services = []string{}
		configurePidDir(t, daemon)

		subject := "spinifex.cluster.shutdown.gate.targeted-here"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownGate))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "gate", Target: daemon.node})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 30*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Equal(t, daemon.node, ack.Node)
		assert.Empty(t, ack.Error)
		assert.True(t, daemon.shuttingDown.Load())
	})

	// Every phase is a fan-out subject, so each handler needs the check —
	// STORAGE and PERSIST stop another node's storage, and INFRA exits its
	// daemon outright.
	t.Run("every phase handler ignores another node's request", func(t *testing.T) {
		daemon := createTestDaemon(t, sharedNATSURL)
		daemon.config.Services = []string{}
		configurePidDir(t, daemon)

		for phase, handler := range map[string]nats.MsgHandler{
			"drain":   asMsgHandler(daemon.handleShutdownDrain),
			"storage": asMsgHandler(daemon.handleShutdownStorage),
			"persist": asMsgHandler(daemon.handleShutdownPersist),
			"infra":   asMsgHandler(daemon.handleShutdownInfra),
		} {
			t.Run(phase, func(t *testing.T) {
				subject := "spinifex.cluster.shutdown." + phase + ".elsewhere"
				sub, err := daemon.natsConn.Subscribe(subject, handler)
				require.NoError(t, err)
				defer sub.Unsubscribe()
				require.NoError(t, daemon.natsConn.Flush())

				payload, err := json.Marshal(ShutdownRequest{Phase: phase, Target: "some-other-node"})
				require.NoError(t, err)

				_, err = daemon.natsConn.Request(subject, payload, noReplyWait)
				require.Error(t, err, "%s answered a request addressed to another node", phase)
			})
		}
	})

	t.Run("untargeted request still reaches every node", func(t *testing.T) {
		daemon := createTestDaemon(t, sharedNATSURL)
		daemon.config.Services = []string{}
		configurePidDir(t, daemon)

		subject := "spinifex.cluster.shutdown.gate.untargeted"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownGate))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "gate"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 30*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Empty(t, ack.Error)
		assert.True(t, daemon.shuttingDown.Load(), "a cluster-wide shutdown must still gate this daemon")
	})
}

// TestCleanupOrphanNBDKit covers the scoped replacement for the host-wide
// pattern kill: only PIDs named by this node's own pidfiles are signalled,
// and only while the kernel still agrees the PID is nbdkit.
func TestCleanupOrphanNBDKit(t *testing.T) {
	writePid := func(t *testing.T, dir, name string, contents string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		return path
	}

	// stubProcTable replaces the /proc lookup and the kill with in-memory
	// equivalents, returning the slice that records what was signalled.
	stubProcTable := func(t *testing.T, comms map[int]string) *[]int {
		t.Helper()
		origComm, origSignal := procComm, signalProcess
		t.Cleanup(func() { procComm, signalProcess = origComm, origSignal })

		var signalled []int
		procComm = func(pid int) (string, error) {
			comm, ok := comms[pid]
			if !ok {
				return "", os.ErrNotExist
			}
			return comm, nil
		}
		signalProcess = func(pid int, _ syscall.Signal) error {
			signalled = append(signalled, pid)
			return nil
		}
		return &signalled
	}

	t.Run("signals only this node's live nbdkit pids", func(t *testing.T) {
		dir := t.TempDir()
		writePid(t, dir, "nbdkit-vol-vol-a.pid", "1001")
		writePid(t, dir, "nbdkit-vol-vol-b.pid", "1002")
		writePid(t, dir, "qemu-i-0123.pid", "1003")

		signalled := stubProcTable(t, map[int]string{1001: "nbdkit", 1002: "nbdkit", 1003: "qemu-system-x86_64"})

		cleanupOrphanNBDKit(dir)

		assert.ElementsMatch(t, []int{1001, 1002}, *signalled, "only nbdkit pidfiles should be swept")
		assert.FileExists(t, filepath.Join(dir, "qemu-i-0123.pid"), "unrelated pid files must be left alone")
	})

	t.Run("skips a recycled pid that is no longer nbdkit", func(t *testing.T) {
		dir := t.TempDir()
		writePid(t, dir, "nbdkit-vol-recycled.pid", "2001")

		signalled := stubProcTable(t, map[int]string{2001: "postgres"})

		cleanupOrphanNBDKit(dir)

		assert.Empty(t, *signalled, "a pid whose comm is not nbdkit must never be signalled")
	})

	t.Run("removes pid files for dead and unparsable entries", func(t *testing.T) {
		dir := t.TempDir()
		dead := writePid(t, dir, "nbdkit-vol-dead.pid", "3001")
		garbage := writePid(t, dir, "nbdkit-vol-garbage.pid", "not-a-pid")

		signalled := stubProcTable(t, map[int]string{})

		cleanupOrphanNBDKit(dir)

		assert.Empty(t, *signalled)
		assert.NoFileExists(t, dead)
		assert.NoFileExists(t, garbage)
	})

	t.Run("empty runtime dir is a no-op", func(t *testing.T) {
		signalled := stubProcTable(t, map[int]string{})
		cleanupOrphanNBDKit("")
		assert.Empty(t, *signalled)
	})
}

// TestProcCommAndSignalProcess exercises the real /proc and kill paths the
// cleanup stubs out everywhere else, so a change to either is caught here
// rather than only on a node.
func TestProcCommAndSignalProcess(t *testing.T) {
	t.Run("procComm reads this process's own name", func(t *testing.T) {
		comm, err := procComm(os.Getpid())
		require.NoError(t, err)
		assert.NotEmpty(t, comm)
		assert.NotContains(t, comm, "\n", "comm must be trimmed")
	})

	t.Run("procComm reports a pid that does not exist", func(t *testing.T) {
		// Above the default pid_max, so it cannot collide with a live process.
		_, err := procComm(1 << 30)
		require.Error(t, err)
	})

	t.Run("signalProcess reaches a live process", func(t *testing.T) {
		pid := startSleepProcess(t)
		// Signal 0 runs every permission check and delivers nothing, which is
		// the reachability probe without disturbing the process.
		require.NoError(t, signalProcess(pid, syscall.Signal(0)))
	})

	t.Run("signalProcess reports a pid that does not exist", func(t *testing.T) {
		require.Error(t, signalProcess(1<<30, syscall.Signal(0)))
	})
}

// TestHandleShutdownDrain covers the DRAIN phase handler: graceful StopAll,
// shutdown marker, state persistence, and progress publishing.
func TestHandleShutdownDrain(t *testing.T) {
	t.Run("happy path stops all vms writes marker and persists state", func(t *testing.T) {
		daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)
		require.NoError(t, daemon.jsManager.InitClusterStateBucket())
		t.Cleanup(func() { _ = daemon.jsManager.DeleteShutdownMarker(daemon.node) })

		daemon.vmMgr.Insert(&vm.VM{ID: "i-drain-001"})

		subject := "spinifex.cluster.shutdown.drain.happy"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownDrain))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "drain"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 30*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Equal(t, "drain", ack.Phase)
		assert.Empty(t, ack.Error)

		marker, err := daemon.jsManager.ReadShutdownMarker(daemon.node)
		require.NoError(t, err)
		assert.True(t, marker, "shutdown marker should be written for node")

		statePath := daemon.localStatePath()
		_, statErr := os.Stat(statePath)
		assert.NoError(t, statErr, "local state file should exist after DRAIN")
	})

	t.Run("empty vm map skips StopAll but still writes marker and state", func(t *testing.T) {
		daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)
		require.NoError(t, daemon.jsManager.InitClusterStateBucket())
		t.Cleanup(func() { _ = daemon.jsManager.DeleteShutdownMarker(daemon.node) })

		subject := "spinifex.cluster.shutdown.drain.empty"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownDrain))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "drain"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 10*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Equal(t, "drain", ack.Phase)
		assert.Empty(t, ack.Error)

		marker, err := daemon.jsManager.ReadShutdownMarker(daemon.node)
		require.NoError(t, err)
		assert.True(t, marker)
	})

	t.Run("malformed json returns error ack with no marker", func(t *testing.T) {
		daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)
		require.NoError(t, daemon.jsManager.InitClusterStateBucket())
		t.Cleanup(func() { _ = daemon.jsManager.DeleteShutdownMarker(daemon.node) })

		subject := "spinifex.cluster.shutdown.drain.malformed"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownDrain))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		reply, err := daemon.natsConn.Request(subject, []byte(`{not json`), 5*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Equal(t, "drain", ack.Phase)
		assert.NotEmpty(t, ack.Error)

		marker, err := daemon.jsManager.ReadShutdownMarker(daemon.node)
		require.NoError(t, err)
		assert.False(t, marker, "no marker should be written when parse fails")
	})

	t.Run("multiple vms publish initial and final progress", func(t *testing.T) {
		daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)
		require.NoError(t, daemon.jsManager.InitClusterStateBucket())
		t.Cleanup(func() { _ = daemon.jsManager.DeleteShutdownMarker(daemon.node) })

		const vmCount = 3
		for i := range vmCount {
			daemon.vmMgr.Insert(&vm.VM{ID: fmt.Sprintf("i-drain-%03d", i)})
		}

		progressSub, err := daemon.natsConn.SubscribeSync("spinifex.cluster.shutdown.progress")
		require.NoError(t, err)
		defer progressSub.Unsubscribe()

		subject := "spinifex.cluster.shutdown.drain.multi"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownDrain))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "drain"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 30*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Empty(t, ack.Error)

		var progresses []ShutdownProgress
		for {
			msg, err := progressSub.NextMsg(500 * time.Millisecond)
			if err != nil {
				break
			}
			var p ShutdownProgress
			require.NoError(t, json.Unmarshal(msg.Data, &p))
			progresses = append(progresses, p)
		}

		require.GreaterOrEqual(t, len(progresses), 2, "expected initial and final progress publishes")
		first := progresses[0]
		last := progresses[len(progresses)-1]

		assert.Equal(t, "drain", first.Phase)
		assert.Equal(t, vmCount, first.Total)
		assert.Equal(t, vmCount, first.Remaining, "initial progress should report all VMs remaining")

		assert.Equal(t, "drain", last.Phase)
		assert.Equal(t, vmCount, last.Total)
		assert.Equal(t, 0, last.Remaining, "final progress should report zero remaining")
	})
}

// stubEBSUnmount answers the daemon's ebs.<node>.unmount subject with a fixed
// response, standing in for viperblockd. Returns the volumes it was asked to
// unmount once the subscription has been drained.
func stubEBSUnmount(t *testing.T, d *Daemon, resp types.EBSUnMountResponse) func() []string {
	t.Helper()
	var (
		mu      sync.Mutex
		volumes []string
	)
	sub, err := d.natsConn.Subscribe("ebs."+d.node+".unmount", func(msg *nats.Msg) {
		var req types.EBSRequest
		require.NoError(t, json.Unmarshal(msg.Data, &req))
		mu.Lock()
		volumes = append(volumes, req.Name)
		mu.Unlock()
		reply := resp
		reply.Volume = req.Name
		data, marshalErr := json.Marshal(reply)
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, d.natsConn.Flush())

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), volumes...)
	}
}

// drainRunningInstance is a guest in the state DRAIN finds it in: running, with
// a boot volume attached, and drain-stopped rather than operator-stopped.
func drainRunningInstance(id, volume string) *vm.VM {
	instance := &vm.VM{ID: id, Status: vm.StateRunning, AccountID: "111122223333"}
	instance.EBSRequests.Requests = []types.EBSRequest{
		{Name: volume, VolType: types.VolumeTypeGP3, Boot: true},
	}
	return instance
}

// TestHandleShutdownDrain_SealFailure drives DRAIN end to end over NATS against
// a fault-injected block store whose seal never completes — the stalled blob
// node that destroyed two guests on upgrade. The phase must fail rather than
// ACK success, so the coordinator refuses to advance to STORAGE and stop
// viperblock over an unsealed block map.
func TestHandleShutdownDrain_SealFailure(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	t.Run("stalled blob node fails the phase", func(t *testing.T) {
		daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)
		require.NoError(t, daemon.jsManager.InitClusterStateBucket())
		t.Cleanup(func() { _ = daemon.jsManager.DeleteShutdownMarker(daemon.node) })

		unmounted := stubEBSUnmount(t, daemon, types.EBSUnMountResponse{
			Mounted: true,
			Error:   "seal volume to predastore: transfer stalled: idle timeout",
		})
		daemon.vmMgr.Insert(drainRunningInstance("i-drain-seal-fail", "vol-drain-seal-fail"))

		subject := "spinifex.cluster.shutdown.drain.sealfail"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownDrain))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "drain"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 60*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Equal(t, "drain", ack.Phase)
		require.NotEmpty(t, ack.Error,
			"DRAIN must ACK with an error when a volume seal failed")
		assert.Contains(t, ack.Error, "vol-drain-seal-fail",
			"the ACK must name the volume the operator has to fix")
		assert.Equal(t, []string{"vol-drain-seal-fail"}, unmounted(),
			"the seal must have been attempted")

		marker, err := daemon.jsManager.ReadShutdownMarker(daemon.node)
		require.NoError(t, err)
		assert.False(t, marker,
			"a failed DRAIN must not record the node as cleanly shut down")
	})

	t.Run("healthy blob node completes the phase", func(t *testing.T) {
		daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)
		require.NoError(t, daemon.jsManager.InitClusterStateBucket())
		t.Cleanup(func() { _ = daemon.jsManager.DeleteShutdownMarker(daemon.node) })

		unmounted := stubEBSUnmount(t, daemon, types.EBSUnMountResponse{})
		instance := drainRunningInstance("i-drain-seal-ok", "vol-drain-seal-ok")
		daemon.vmMgr.Insert(instance)

		subject := "spinifex.cluster.shutdown.drain.sealok"
		sub, err := daemon.natsConn.Subscribe(subject, asMsgHandler(daemon.handleShutdownDrain))
		require.NoError(t, err)
		defer sub.Unsubscribe()
		require.NoError(t, daemon.natsConn.Flush())

		payload, err := json.Marshal(ShutdownRequest{Phase: "drain"})
		require.NoError(t, err)

		reply, err := daemon.natsConn.Request(subject, payload, 60*time.Second)
		require.NoError(t, err)

		var ack ShutdownACK
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Equal(t, "drain", ack.Phase)
		assert.Empty(t, ack.Error, "a sealed volume must not fail DRAIN")
		assert.Equal(t, []string{"vol-drain-seal-ok"}, unmounted())
		assert.Equal(t, vm.StateStopped, daemon.vmMgr.Status(instance))

		marker, err := daemon.jsManager.ReadShutdownMarker(daemon.node)
		require.NoError(t, err)
		assert.True(t, marker)
	})
}
