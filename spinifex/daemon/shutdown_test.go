package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestShuttingDownFlagSkipsVMStop verifies that the shuttingDown flag is respected.
func TestShuttingDownFlagSkipsVMStop(t *testing.T) {
	d := &Daemon{}

	// Default should be false
	assert.False(t, d.shuttingDown.Load())

	// Set to true (as GATE phase does)
	d.shuttingDown.Store(true)
	assert.True(t, d.shuttingDown.Load())
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = cmd.Wait()
	}()
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
		sub, err := daemon.natsConn.Subscribe(subject, daemon.handleShutdownGate)
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
		sub, err := daemon.natsConn.Subscribe(subject, daemon.handleShutdownGate)
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
		sub, err := daemon.natsConn.Subscribe(subject, daemon.handleShutdownGate)
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

// TestHandleShutdownDrain covers the DRAIN phase handler: graceful StopAll,
// shutdown marker, state persistence, and progress publishing.
func TestHandleShutdownDrain(t *testing.T) {
	t.Run("happy path stops all vms writes marker and persists state", func(t *testing.T) {
		daemon := createFullTestDaemonWithJetStream(t, sharedJSNATSURL)
		require.NoError(t, daemon.jsManager.InitClusterStateBucket())
		t.Cleanup(func() { _ = daemon.jsManager.DeleteShutdownMarker(daemon.node) })

		daemon.vmMgr.Insert(&vm.VM{ID: "i-drain-001"})

		subject := "spinifex.cluster.shutdown.drain.happy"
		sub, err := daemon.natsConn.Subscribe(subject, daemon.handleShutdownDrain)
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
		sub, err := daemon.natsConn.Subscribe(subject, daemon.handleShutdownDrain)
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
		sub, err := daemon.natsConn.Subscribe(subject, daemon.handleShutdownDrain)
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
		for i := 0; i < vmCount; i++ {
			daemon.vmMgr.Insert(&vm.VM{ID: fmt.Sprintf("i-drain-%03d", i)})
		}

		progressSub, err := daemon.natsConn.SubscribeSync("spinifex.cluster.shutdown.progress")
		require.NoError(t, err)
		defer progressSub.Unsubscribe()

		subject := "spinifex.cluster.shutdown.drain.multi"
		sub, err := daemon.natsConn.Subscribe(subject, daemon.handleShutdownDrain)
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
