package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHookTestDaemon returns a Daemon with the minimum surface required to
// drive onInstanceUpHook / onInstanceDownHook: a live NATS connection and an
// initialised natsSubscriptions map. The hook handlers themselves are never
// invoked here — the assertions inspect the subscription map and topics.
func newHookTestDaemon(t *testing.T) (*Daemon, *nats.Conn) {
	t.Helper()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d := &Daemon{
		natsConn:          nc,
		natsSubscriptions: make(map[string]*nats.Subscription),
	}
	return d, nc
}

func TestOnInstanceUpHook_RegistersBothPerInstanceTopics(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-basic"}

	require.NoError(t, d.onInstanceUpHook()(instance))

	cmdSub, ok := d.natsSubscriptions[instance.ID]
	require.True(t, ok, "ec2.cmd subscription must be registered under instance ID")
	assert.Equal(t, "ec2.cmd.i-up-basic", cmdSub.Subject)

	consoleSub, ok := d.natsSubscriptions[instance.ID+".console"]
	require.True(t, ok, "console subscription must be registered under <id>.console key")
	assert.Equal(t, "ec2.i-up-basic.GetConsoleOutput", consoleSub.Subject)
}

func TestOnInstanceUpHook_ReplacesExistingSubsOnDoubleUp(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-twice"}

	require.NoError(t, d.onInstanceUpHook()(instance))
	first := d.natsSubscriptions[instance.ID]
	firstConsole := d.natsSubscriptions[instance.ID+".console"]
	require.NotNil(t, first)
	require.NotNil(t, firstConsole)

	require.NoError(t, d.onInstanceUpHook()(instance))
	second := d.natsSubscriptions[instance.ID]
	secondConsole := d.natsSubscriptions[instance.ID+".console"]
	require.NotNil(t, second)
	require.NotNil(t, secondConsole)

	// Second call must have unsubscribed the originals (so they're no longer
	// receiving on the topic) and replaced the map entries with fresh subs.
	assert.False(t, first.IsValid(), "first command sub should be unsubscribed")
	assert.False(t, firstConsole.IsValid(), "first console sub should be unsubscribed")
	assert.True(t, second.IsValid(), "second command sub should be live")
	assert.True(t, secondConsole.IsValid(), "second console sub should be live")
	assert.NotSame(t, first, second, "command sub map entry must be replaced")
	assert.NotSame(t, firstConsole, secondConsole, "console sub map entry must be replaced")
}

func TestOnInstanceDownHook_UnsubscribesAndDeletes(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-down"}

	require.NoError(t, d.onInstanceUpHook()(instance))
	cmdSub := d.natsSubscriptions[instance.ID]
	consoleSub := d.natsSubscriptions[instance.ID+".console"]
	require.NotNil(t, cmdSub)
	require.NotNil(t, consoleSub)

	d.onInstanceDownHook()(instance.ID)

	_, cmdPresent := d.natsSubscriptions[instance.ID]
	_, consolePresent := d.natsSubscriptions[instance.ID+".console"]
	assert.False(t, cmdPresent, "command sub must be deleted from map")
	assert.False(t, consolePresent, "console sub must be deleted from map")
	assert.False(t, cmdSub.IsValid(), "command sub must be unsubscribed")
	assert.False(t, consoleSub.IsValid(), "console sub must be unsubscribed")
}

func TestOnInstanceDownHook_NoOpWhenAbsent(t *testing.T) {
	d, _ := newHookTestDaemon(t)

	// Down on an unknown instance must not panic and must leave the map empty.
	d.onInstanceDownHook()("i-never-up")

	assert.Empty(t, d.natsSubscriptions)
}

// When the daemon's gpuManager is unset, the hook must still register NATS
// subscriptions and ignore the GPUPCIAddress on the VM.
func TestOnInstanceUpHook_NoGPUManager_SkipsReclaim(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-nogpu", GPUPCIAddress: "0000:01:00.0"}

	require.NoError(t, d.onInstanceUpHook()(instance))
	require.Contains(t, d.natsSubscriptions, instance.ID)
}

// With a gpuManager configured but a CPU-only instance, the hook must not
// touch the GPU pool. We use the AllocatedCount as the observable: an
// unintended Reclaim would bump it.
func TestOnInstanceUpHook_NoGPUAddress_SkipsReclaim(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	d.gpuManager = gpu.NewManager(nil)
	instance := &vm.VM{ID: "i-up-cpu"}

	require.NoError(t, d.onInstanceUpHook()(instance))
	assert.Equal(t, 0, d.gpuManager.AllocatedCount(),
		"hook must not call Reclaim for instances without a GPUPCIAddress")
}

// With a gpuManager that has no entries, calling Reclaim for an instance
// with a GPUPCIAddress will fail inside the manager. The hook logs a warning
// and returns nil — the NATS subscriptions must still register so the
// reconnect path doesn't roll back.
func TestOnInstanceUpHook_GPUReclaimError_DoesNotPropagate(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	d.gpuManager = gpu.NewManager(nil)
	instance := &vm.VM{ID: "i-up-gpu-missing", GPUPCIAddress: "0000:99:00.0"}

	require.NoError(t, d.onInstanceUpHook()(instance))
	require.Contains(t, d.natsSubscriptions, instance.ID)
}

func TestOnInstanceDownHook_OnlyRemovesTargetedInstance(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	keep := &vm.VM{ID: "i-keep"}
	drop := &vm.VM{ID: "i-drop"}

	require.NoError(t, d.onInstanceUpHook()(keep))
	require.NoError(t, d.onInstanceUpHook()(drop))
	require.Len(t, d.natsSubscriptions, 4)

	d.onInstanceDownHook()(drop.ID)

	assert.Len(t, d.natsSubscriptions, 2)
	assert.NotNil(t, d.natsSubscriptions[keep.ID])
	assert.NotNil(t, d.natsSubscriptions[keep.ID+".console"])
	_, dropPresent := d.natsSubscriptions[drop.ID]
	assert.False(t, dropPresent)
}
