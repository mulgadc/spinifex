package daemon

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/types"
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

func TestOnInstanceUpHook_ReArmsSystemTerminateForELBv2(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-sys", ManagedBy: tags.ManagedByELBv2}

	require.NoError(t, d.onInstanceUpHook()(instance))

	// OnInstanceUp fires on both the relaunch and reconnect recovery paths, so
	// the terminate subject must be re-bound or the recovered LB VM can never
	// be torn down (SetSubnets relaunch, DeleteLoadBalancer).
	sub, ok := d.natsSubscriptions["system.TerminateInstance.i-up-sys"]
	require.True(t, ok, "system terminate subscription must be re-armed for ELBv2 VMs")
	assert.Equal(t, "system.TerminateInstance.i-up-sys", sub.Subject)
}

// TestOnInstanceUpHook_ReArmsSystemTerminateForEKS locks the contract: an EKS
// K3s control-plane VM placed on the coordinator's own node (local launch, no
// remote-launch handler) must still bind system.TerminateInstance.{id} via the
// OnInstanceUp funnel — otherwise a cluster-wide teardown invoked on another node
// finds no responder, treats the VM as gone, and deletes its still-attached ENI.
func TestOnInstanceUpHook_ReArmsSystemTerminateForEKS(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-eks", ManagedBy: tags.ManagedByEKS}

	require.NoError(t, d.onInstanceUpHook()(instance))

	sub, ok := d.natsSubscriptions["system.TerminateInstance.i-up-eks"]
	require.True(t, ok, "system terminate subscription must be bound for EKS control-plane VMs")
	assert.Equal(t, "system.TerminateInstance.i-up-eks", sub.Subject)
}

func TestOnInstanceUpHook_NoSystemTerminateForRegularInstance(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-regular"}

	require.NoError(t, d.onInstanceUpHook()(instance))

	_, ok := d.natsSubscriptions["system.TerminateInstance.i-up-regular"]
	assert.False(t, ok, "regular instances must not register a system terminate subscription")
}

func TestOnInstanceDownHook_DropsSystemTerminateSubscription(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-down-sys", ManagedBy: tags.ManagedByELBv2}

	require.NoError(t, d.onInstanceUpHook()(instance))
	termSub := d.natsSubscriptions["system.TerminateInstance.i-down-sys"]
	require.NotNil(t, termSub)

	d.onInstanceDownHook()(instance.ID)

	_, present := d.natsSubscriptions["system.TerminateInstance.i-down-sys"]
	assert.False(t, present, "system terminate sub must be deleted from map")
	assert.False(t, termSub.IsValid(), "system terminate sub must be unsubscribed")
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
// subscriptions and ignore the GPUPCIAddresses on the VM.
func TestOnInstanceUpHook_NoGPUManager_SkipsReclaim(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-nogpu", GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:01:00.0"}}}

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
		"hook must not call Reclaim for instances without GPUPCIAddresses")
}

// With a gpuManager that has no entries, calling Reclaim for an instance
// with a GPUPCIAddresses entry will fail inside the manager. The hook logs a warning
// and returns nil — the NATS subscriptions must still register so the
// reconnect path doesn't roll back.
func TestOnInstanceUpHook_GPUReclaimError_DoesNotPropagate(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	d.gpuManager = gpu.NewManager(nil)
	instance := &vm.VM{ID: "i-up-gpu-missing", GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:99:00.0"}}}

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

func TestVolumeMounterAdapter_MountOne(t *testing.T) {
	tests := []struct {
		name       string
		responder  func(t *testing.T, msg *nats.Msg)
		skipSub    bool
		wantErr    bool
		wantErrSub string
		wantErrIs  error
		wantNBDURI string
		initialURI string
	}{
		{
			name: "HappyPath_UpdatesNBDURI",
			responder: func(t *testing.T, msg *nats.Msg) {
				resp := types.EBSMountResponse{URI: "nbd://mounted-vol"}
				data, err := json.Marshal(resp)
				require.NoError(t, err)
				require.NoError(t, msg.Respond(data))
			},
			wantNBDURI: "nbd://mounted-vol",
		},
		{
			name:       "NATSNoResponders_ReturnsError",
			skipSub:    true,
			wantErr:    true,
			wantErrSub: "ebs.mount NATS request",
		},
		{
			name: "UnmarshalFailure_ReturnsError",
			responder: func(t *testing.T, msg *nats.Msg) {
				require.NoError(t, msg.Respond([]byte("not json")))
			},
			wantErr:    true,
			wantErrSub: "unmarshal ebs.mount response",
		},
		{
			name: "ResponseError_IncludedInError",
			responder: func(t *testing.T, msg *nats.Msg) {
				resp := types.EBSMountResponse{Error: "boom"}
				data, err := json.Marshal(resp)
				require.NoError(t, err)
				require.NoError(t, msg.Respond(data))
			},
			wantErr:    true,
			wantErrSub: "boom",
		},
		{
			name: "EmptyURI_ReturnsErrMountAmbiguous",
			responder: func(t *testing.T, msg *nats.Msg) {
				resp := types.EBSMountResponse{URI: ""}
				data, err := json.Marshal(resp)
				require.NoError(t, err)
				require.NoError(t, msg.Respond(data))
			},
			wantErr:   true,
			wantErrIs: vm.ErrMountAmbiguous,
		},
		{
			name: "EmptyURI_PreservesInitialNBDURIOnFailure",
			responder: func(t *testing.T, msg *nats.Msg) {
				resp := types.EBSMountResponse{URI: ""}
				data, err := json.Marshal(resp)
				require.NoError(t, err)
				require.NoError(t, msg.Respond(data))
			},
			initialURI: "nbd://stale",
			wantErr:    true,
			wantErrIs:  vm.ErrMountAmbiguous,
			wantNBDURI: "nbd://stale",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon := createTestDaemon(t, sharedNATSURL)
			adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)

			if !tt.skipSub {
				sub, err := daemon.natsConn.Subscribe("ebs.node-1.mount", func(msg *nats.Msg) {
					tt.responder(t, msg)
				})
				require.NoError(t, err)
				defer sub.Unsubscribe()
			}

			req := &types.EBSRequest{
				Name:       "vol-mountone",
				DeviceName: "/dev/sdf",
				NBDURI:     tt.initialURI,
			}

			err := adapter.MountOne(req)
			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrSub != "" {
					assert.Contains(t, err.Error(), tt.wantErrSub)
				}
				if tt.wantErrIs != nil {
					assert.ErrorIs(t, err, tt.wantErrIs)
				}
				assert.Equal(t, tt.wantNBDURI, req.NBDURI,
					"NBDURI must not be overwritten on failure")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantNBDURI, req.NBDURI,
				"happy path must write resolved NBDURI back to req")
		})
	}
}

// --- ReleaseGPU ---

// ReleaseGPU is a no-op when the daemon has no GPU manager.
func TestReleaseGPU_NoManager_NoOp(t *testing.T) {
	d := &Daemon{}
	a := newInstanceCleanerAdapter(d)
	instance := &vm.VM{ID: "i-nogpu", GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:03:00.0"}}}
	// Must not panic.
	a.ReleaseGPU(instance)
}

// ReleaseGPU is a no-op for instances without a GPU allocation.
func TestReleaseGPU_NoAddresses_NoOp(t *testing.T) {
	mgr := gpu.NewManager(nil)
	d := &Daemon{gpuManager: mgr}
	a := newInstanceCleanerAdapter(d)
	instance := &vm.VM{ID: "i-nogpu"}
	// Must not panic or error.
	a.ReleaseGPU(instance)
}

// ReleaseGPU logs a warning when the manager returns an error (instance not claimed).
func TestReleaseGPU_ManagerError_LogsWarning(t *testing.T) {
	mgr := gpu.NewManager(nil)
	d := &Daemon{gpuManager: mgr}
	a := newInstanceCleanerAdapter(d)
	// GPU address set but no claim registered — Release returns an error.
	instance := &vm.VM{ID: "i-unclaimed", GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:03:00.0"}}}
	// Must not panic; the error is logged as a warning.
	a.ReleaseGPU(instance)
}

// TestBuildVMManagerDeps_WiresBeforeInstanceRelaunch guards the single line
// in buildVMManagerDeps that routes the recovery hook to
// refreshSystemInstanceState. Dropping it would surface only in cell-18.
func TestBuildVMManagerDeps_WiresBeforeInstanceRelaunch(t *testing.T) {
	d := &Daemon{config: &config.Config{}, vmMgr: vm.NewManager()}
	deps := d.buildVMManagerDeps()
	require.NotNil(t, deps.Hooks.BeforeInstanceRelaunch)

	wantPC := reflect.ValueOf(d.refreshSystemInstanceState).Pointer()
	gotPC := reflect.ValueOf(deps.Hooks.BeforeInstanceRelaunch).Pointer()
	assert.Equal(t, wantPC, gotPC, "hook must point at refreshSystemInstanceState")

	// Sanity: the wired hook is callable and returns nil for non-ELBv2 VMs.
	require.NoError(t, deps.Hooks.BeforeInstanceRelaunch(&vm.VM{ID: "i-noop", ManagedBy: ""}))
	require.Error(t, deps.Hooks.BeforeInstanceRelaunch(&vm.VM{ID: "i-svc", ManagedBy: tags.ManagedByELBv2}),
		"ELBv2 VM with nil elbv2Service must error rather than silently no-op")
}
