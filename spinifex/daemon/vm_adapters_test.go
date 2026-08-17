package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/testutil"
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

func TestOnInstanceUpHook_RegistersAllPerInstanceTopics(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-up-basic"}

	require.NoError(t, d.onInstanceUpHook()(instance))

	cmdSub, ok := d.natsSubscriptions[instance.ID]
	require.True(t, ok, "ec2.cmd subscription must be registered under instance ID")
	assert.Equal(t, "ec2.cmd.i-up-basic", cmdSub.Subject)

	consoleSub, ok := d.natsSubscriptions[instance.ID+".console"]
	require.True(t, ok, "console subscription must be registered under <id>.console key")
	assert.Equal(t, "ec2.i-up-basic.GetConsoleOutput", consoleSub.Subject)

	passwordSub, ok := d.natsSubscriptions[instance.ID+".password"]
	require.True(t, ok, "password subscription must be registered under <id>.password key")
	assert.Equal(t, "ec2.i-up-basic.GetPasswordData", passwordSub.Subject)
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
	firstPassword := d.natsSubscriptions[instance.ID+".password"]
	require.NotNil(t, first)
	require.NotNil(t, firstConsole)
	require.NotNil(t, firstPassword)

	require.NoError(t, d.onInstanceUpHook()(instance))
	second := d.natsSubscriptions[instance.ID]
	secondConsole := d.natsSubscriptions[instance.ID+".console"]
	secondPassword := d.natsSubscriptions[instance.ID+".password"]
	require.NotNil(t, second)
	require.NotNil(t, secondConsole)
	require.NotNil(t, secondPassword)

	// Second call must have unsubscribed the originals (so they're no longer
	// receiving on the topic) and replaced the map entries with fresh subs.
	assert.False(t, first.IsValid(), "first command sub should be unsubscribed")
	assert.False(t, firstConsole.IsValid(), "first console sub should be unsubscribed")
	assert.False(t, firstPassword.IsValid(), "first password sub should be unsubscribed")
	assert.True(t, second.IsValid(), "second command sub should be live")
	assert.True(t, secondConsole.IsValid(), "second console sub should be live")
	assert.True(t, secondPassword.IsValid(), "second password sub should be live")
	assert.NotSame(t, first, second, "command sub map entry must be replaced")
	assert.NotSame(t, firstConsole, secondConsole, "console sub map entry must be replaced")
	assert.NotSame(t, firstPassword, secondPassword, "password sub map entry must be replaced")
}

func TestOnInstanceDownHook_UnsubscribesAndDeletes(t *testing.T) {
	d, _ := newHookTestDaemon(t)
	instance := &vm.VM{ID: "i-down"}

	require.NoError(t, d.onInstanceUpHook()(instance))
	cmdSub := d.natsSubscriptions[instance.ID]
	consoleSub := d.natsSubscriptions[instance.ID+".console"]
	passwordSub := d.natsSubscriptions[instance.ID+".password"]
	require.NotNil(t, cmdSub)
	require.NotNil(t, consoleSub)
	require.NotNil(t, passwordSub)

	d.onInstanceDownHook()(instance.ID)

	_, cmdPresent := d.natsSubscriptions[instance.ID]
	_, consolePresent := d.natsSubscriptions[instance.ID+".console"]
	_, passwordPresent := d.natsSubscriptions[instance.ID+".password"]
	assert.False(t, cmdPresent, "command sub must be deleted from map")
	assert.False(t, consolePresent, "console sub must be deleted from map")
	assert.False(t, passwordPresent, "password sub must be deleted from map")
	assert.False(t, cmdSub.IsValid(), "command sub must be unsubscribed")
	assert.False(t, consoleSub.IsValid(), "console sub must be unsubscribed")
	assert.False(t, passwordSub.IsValid(), "password sub must be unsubscribed")
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
	require.Len(t, d.natsSubscriptions, 6)

	d.onInstanceDownHook()(drop.ID)

	assert.Len(t, d.natsSubscriptions, 3)
	assert.NotNil(t, d.natsSubscriptions[keep.ID])
	assert.NotNil(t, d.natsSubscriptions[keep.ID+".console"])
	assert.NotNil(t, d.natsSubscriptions[keep.ID+".password"])
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

// TestReleaseGPU_AlreadyReleased_IsSuccess pins the fix for a teardown record
// that could never complete. A stop releases the GPU, so the later terminate
// finds no claim; reporting that as failure left the gpu teardown mark stuck
// short of done, so the record was never purged and the GC re-drove it every
// two minutes indefinitely, failing identically each time.
func TestReleaseGPU_AlreadyReleased_IsSuccess(t *testing.T) {
	mgr := gpu.NewManager(nil)
	d := &Daemon{gpuManager: mgr}
	a := newInstanceCleanerAdapter(d)
	// GPU address set but no claim registered — the shape a re-driven release
	// sees once the work is already done.
	instance := &vm.VM{ID: "i-unclaimed", GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:03:00.0"}}}

	require.NoError(t, a.ReleaseGPU(instance), "an already-released GPU must not fail teardown")
	// Idempotent under repetition, which is what the reaper does to it.
	require.NoError(t, a.ReleaseGPU(instance))
}

// --- RemoveFromSpotRequest ---

// RemoveFromSpotRequest is a no-op when the spot instance service is not
// configured. The service-present path is just a delegation to the spot
// service's CloseForInstance, which is covered by the service's own tests.
func TestRemoveFromSpotRequest_NoService_NoOp(t *testing.T) {
	d := &Daemon{}
	a := newInstanceCleanerAdapter(d)
	require.NoError(t, a.RemoveFromSpotRequest(&vm.VM{ID: "i-x", AccountID: "111111111111"}))
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

// --- DetachAndDeleteENI: post-launch attach enumeration ---

// TestInstanceCleanerAdapter_DetachAndDeleteENI_ReleasesPostLaunchAttach locks
// in the terminate-time read path: DetachAndDeleteENI must enumerate the
// spinifex-vpc-enis KV by InstanceId rather than trusting the launch-time
// instance.ENIId scalar alone. handleAttachNetworkInterface (the real
// post-launch/hot-plug attach path) only ever mutates the KV record, never
// vm.VM — so without the enumeration this ENI would survive terminate and
// pin its SG/subnet/VPC behind DependencyViolation, exactly as observed on
// env19.
func TestInstanceCleanerAdapter_DetachAndDeleteENI_ReleasesPostLaunchAttach(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID

	// Attach via the KV only — instance.ENIId is deliberately left unset,
	// mirroring a hot-plug attach that never touches vm.VM.
	_, err := f.daemon.vpcService.AttachENI(testAccountID, f.eniID, f.vmInst.ID, 1)
	require.NoError(t, err)
	require.Empty(t, f.vmInst.ENIId, "precondition: launch-time scalar must stay unset")

	cleaner := newInstanceCleanerAdapter(f.daemon)
	require.NoError(t, cleaner.DetachAndDeleteENI(f.vmInst))

	_, err = f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.Error(t, err, "the ENI record must be released even though instance.ENIId was never set")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceIDNotFound))
}

// TestInstanceCleanerAdapter_DetachAndDeleteENI_DeleteOnTerminationFalseDetachesOnly
// proves the sweep honours DeleteOnTermination=false: the ENI is detached
// (freed for reattachment) but not deleted, matching AWS semantics.
func TestInstanceCleanerAdapter_DetachAndDeleteENI_DeleteOnTerminationFalseDetachesOnly(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID

	_, err := f.daemon.vpcService.AttachENI(testAccountID, f.eniID, f.vmInst.ID, 1)
	require.NoError(t, err)
	require.NoError(t, f.daemon.vpcService.UpdateENI(testAccountID, f.eniID, func(r *handlers_ec2_vpc.ENIRecord) {
		r.DeleteOnTermination = aws.Bool(false)
	}))

	cleaner := newInstanceCleanerAdapter(f.daemon)
	require.NoError(t, cleaner.DetachAndDeleteENI(f.vmInst))

	rec, err := f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.NoError(t, err, "DeleteOnTermination=false must detach, not delete")
	assert.Equal(t, "available", rec.Status)
	assert.Empty(t, rec.InstanceId)
}

// TestInstanceCleanerAdapter_DetachAndDeleteENI_PrimaryENIReleased covers the
// launch-time instance.ENIId path (as opposed to the KV enumeration sweep):
// a still-attached primary ENI must be detached and force-deleted so it
// converges to NotFound.
func TestInstanceCleanerAdapter_DetachAndDeleteENI_PrimaryENIReleased(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID
	f.vmInst.ENIId = f.eniID

	_, err := f.daemon.vpcService.AttachENI(testAccountID, f.eniID, f.vmInst.ID, 0)
	require.NoError(t, err)

	cleaner := newInstanceCleanerAdapter(f.daemon)
	require.NoError(t, cleaner.DetachAndDeleteENI(f.vmInst))

	_, err = f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.Error(t, err, "the primary ENI must be deleted")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInvalidNetworkInterfaceIDNotFound))
}

// TestInstanceCleanerAdapter_DetachAndDeleteENI_PrimaryENIDetachFailureContinues
// proves a failed detach on the primary ENI does not abort the delete: DetachENI
// on an ENI ID absent from the KV (e.g. already reaped) fails, but
// ForceDeleteInstanceENI tolerates NotFound, so terminate still converges.
func TestInstanceCleanerAdapter_DetachAndDeleteENI_PrimaryENIDetachFailureContinues(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID
	f.vmInst.ENIId = "eni-never-existed"

	cleaner := newInstanceCleanerAdapter(f.daemon)
	require.NoError(t, cleaner.DetachAndDeleteENI(f.vmInst),
		"a failed detach on a missing primary ENI must not fail terminate")
}

// TestInstanceCleanerAdapter_DetachAndDeleteENI_MultipleAttachedENIsReleased locks
// in the fix's core scenario: several post-launch-attached ENIs on one instance
// are all swept, each honouring its own DeleteOnTermination value.
func TestInstanceCleanerAdapter_DetachAndDeleteENI_MultipleAttachedENIsReleased(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID

	eniOut2, err := f.daemon.vpcService.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(f.subnetID),
	}, testAccountID)
	require.NoError(t, err)
	eniID2 := *eniOut2.NetworkInterface.NetworkInterfaceId

	// eniID keeps the fixture's default DeleteOnTermination=true (deleted);
	// eniID2 is explicitly false (detached only).
	_, err = f.daemon.vpcService.AttachENI(testAccountID, f.eniID, f.vmInst.ID, 1)
	require.NoError(t, err)
	_, err = f.daemon.vpcService.AttachENI(testAccountID, eniID2, f.vmInst.ID, 2)
	require.NoError(t, err)
	require.NoError(t, f.daemon.vpcService.UpdateENI(testAccountID, eniID2, func(r *handlers_ec2_vpc.ENIRecord) {
		r.DeleteOnTermination = aws.Bool(false)
	}))

	cleaner := newInstanceCleanerAdapter(f.daemon)
	require.NoError(t, cleaner.DetachAndDeleteENI(f.vmInst))

	_, err = f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.Error(t, err, "DeleteOnTermination=true ENI must be deleted")

	rec2, err := f.daemon.vpcService.GetENIRecord(testAccountID, eniID2)
	require.NoError(t, err, "DeleteOnTermination=false ENI must survive, detached")
	assert.Equal(t, "available", rec2.Status)
}

// TestInstanceCleanerAdapter_DetachAndDeleteENI_AbsentPrimaryDoesNotLogFalseSuccess
// proves the false-success log is gone: a primary ENI that DetachAndDeleteENI
// finds already absent must be logged as absent, never as "Deleted ENI on
// termination" — that log used to fire unconditionally on a nil error, even
// when the force path's stale-NotFound tolerance meant nothing was deleted.
func TestInstanceCleanerAdapter_DetachAndDeleteENI_AbsentPrimaryDoesNotLogFalseSuccess(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID
	f.vmInst.ENIId = "eni-never-existed"

	buf := captureSlogForTest(t)
	cleaner := newInstanceCleanerAdapter(f.daemon)
	require.NoError(t, cleaner.DetachAndDeleteENI(f.vmInst))

	assert.NotContains(t, buf.String(), "Deleted ENI on termination",
		"an already-absent ENI must never be logged as deleted")
	assert.Contains(t, buf.String(), "ENI already absent on termination")
}

// TestInstanceCleanerAdapter_ReleaseAttachedENIs_ListInstanceENIsErrorTolerated
// exercises the enumeration error branch: the connection backing vpcService's
// KV is closed before terminate runs, so ListInstanceENIs fails with a real
// connection error. The sweep must log and return rather than panic or
// propagate the error as the primary terminate failure.
func TestInstanceCleanerAdapter_ReleaseAttachedENIs_ListInstanceENIsErrorTolerated(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), daemon.config, nc)
	require.NoError(t, err)
	daemon.vpcService = vpcSvc
	nc.Close()

	cleaner := newInstanceCleanerAdapter(daemon)
	instance := &vm.VM{ID: "i-kv-down", AccountID: testAccountID}

	require.NoError(t, cleaner.DetachAndDeleteENI(instance),
		"an enumeration failure must not surface as a terminate error")
}

// TestVolumeMounterAdapter_Mount_WrapsErrMountRetryable verifies that a
// viperblockd ebs.mount response with Retryable=true is wrapped so
// vm.Manager's recovery-relaunch retry can match it with errors.Is. A
// Retryable=false response must NOT match, so relaunchAll still fails fast
// on permanent mount errors.
func TestVolumeMounterAdapter_Mount_WrapsErrMountRetryable(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)

	sub, err := daemon.natsConn.Subscribe("ebs."+daemon.node+".mount", func(msg *nats.Msg) {
		resp := types.EBSMountResponse{Error: "state not found", Retryable: true}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instance := &vm.VM{ID: "i-mount-retryable"}
	instance.EBSRequests.Requests = []types.EBSRequest{{Name: "vol-retryable"}}

	err = adapter.Mount(instance)
	require.Error(t, err)
	assert.ErrorIs(t, err, vm.ErrMountRetryable,
		"a Retryable mount response must wrap vm.ErrMountRetryable so relaunchAll can retry it")
}

func TestVolumeMounterAdapter_Mount_PermanentErrorNotWrapped(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)

	sub, err := daemon.natsConn.Subscribe("ebs."+daemon.node+".mount", func(msg *nats.Msg) {
		resp := types.EBSMountResponse{Error: "volume vol-permanent is already mounted read_only=true on this node"}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	instance := &vm.VM{ID: "i-mount-permanent"}
	instance.EBSRequests.Requests = []types.EBSRequest{{Name: "vol-permanent"}}

	err = adapter.Mount(instance)
	require.Error(t, err)
	assert.NotErrorIs(t, err, vm.ErrMountRetryable,
		"a non-Retryable mount response must not be mistaken for a transient failure")
}

func TestVolumeMounterAdapter_Mount_NoResponderIsRetryable(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)

	// No subscriber on the mount subject: viperblockd is still starting, so
	// the request returns nats.ErrNoResponders, which must be retryable.
	instance := &vm.VM{ID: "i-mount-noresponder"}
	instance.EBSRequests.Requests = []types.EBSRequest{{Name: "vol-noresponder"}}

	err := adapter.Mount(instance)
	require.Error(t, err)
	assert.ErrorIs(t, err, vm.ErrMountRetryable,
		"a mount request with no responder must wrap vm.ErrMountRetryable so relaunchAll can retry it")
}
