package daemon

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// stateStoreAdapter satisfies vm.StateStore by delegating to JetStreamManager.
// Tolerates nil JetStreamManager during early boot.
type stateStoreAdapter struct {
	js *JetStreamManager
}

var _ vm.StateStore = (*stateStoreAdapter)(nil)

func newStateStoreAdapter(js *JetStreamManager) *stateStoreAdapter {
	return &stateStoreAdapter{js: js}
}

func (a *stateStoreAdapter) SaveRunningState(nodeID string, snapshot map[string]*vm.VM) error {
	return a.js.WriteState(nodeID, snapshot)
}

func (a *stateStoreAdapter) LoadRunningState(nodeID string) (map[string]*vm.VM, error) {
	return a.js.LoadState(nodeID)
}

func (a *stateStoreAdapter) WriteStoppedInstance(id string, v *vm.VM) error {
	return a.js.WriteStoppedInstance(id, v)
}

func (a *stateStoreAdapter) LoadStoppedInstance(id string) (*vm.VM, error) {
	return a.js.LoadStoppedInstance(id)
}

func (a *stateStoreAdapter) DeleteStoppedInstance(id string) error {
	return a.js.DeleteStoppedInstance(id)
}

func (a *stateStoreAdapter) ListStoppedInstances() ([]*vm.VM, error) {
	return a.js.ListStoppedInstances()
}

func (a *stateStoreAdapter) WriteTerminatedInstance(id string, v *vm.VM) error {
	return a.js.WriteTerminatedInstance(id, v)
}

func (a *stateStoreAdapter) ListTerminatedInstances() ([]*vm.VM, error) {
	return a.js.ListTerminatedInstances()
}

func (a *stateStoreAdapter) DeleteTerminatedInstance(id string) error {
	return a.js.DeleteTerminatedInstance(id)
}

// volumeMounterAdapter satisfies vm.VolumeMounter by routing ebs.mount /
// ebs.unmount NATS requests. Unmount also updates volume state via VolumeStateUpdater.
type volumeMounterAdapter struct {
	nc       *nats.Conn
	node     string
	volState vm.VolumeStateUpdater
}

var _ vm.VolumeMounter = (*volumeMounterAdapter)(nil)

// unmountSealTimeout bounds the ebs.unmount NATS request. The handler now drives
// a synchronous block-map seal to predastore (S3 I/O), so it matches
// viperblockd's 120s KillProcess wait rather than a bare RPC budget.
const unmountSealTimeout = 120 * time.Second

func newVolumeMounterAdapter(nc *nats.Conn, node string, volState vm.VolumeStateUpdater) *volumeMounterAdapter {
	return &volumeMounterAdapter{nc: nc, node: node, volState: volState}
}

func (a *volumeMounterAdapter) topic(action string) string {
	return fmt.Sprintf("ebs.%s.%s", a.node, action)
}

func (a *volumeMounterAdapter) Mount(instance *vm.VM) error {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()

	mounted := make([]int, 0, len(instance.EBSRequests.Requests))
	rollback := func(origErr error) error {
		var rbErrs []error
		for _, idx := range mounted {
			req := instance.EBSRequests.Requests[idx]
			if err := a.unmountOne(req); err != nil {
				slog.Error("Mount rollback: unmount failed",
					"volume", req.Name, "err", err)
				rbErrs = append(rbErrs, fmt.Errorf("unmount %s: %w", req.Name, err))
			}
		}
		if len(rbErrs) > 0 {
			return fmt.Errorf("%w; rollback also failed: %w", origErr, errors.Join(rbErrs...))
		}
		return origErr
	}

	for k, v := range instance.EBSRequests.Requests {
		ebsMountRequest, err := json.Marshal(v)
		if err != nil {
			slog.Error("Failed to marshal volume payload", "err", err)
			return rollback(err)
		}

		reply, err := a.nc.Request(a.topic("mount"), ebsMountRequest, 30*time.Second)

		slog.Info("Mounting volume", "Vol", v.Name, "NBDURI", v.NBDURI)

		if err != nil {
			slog.Error("Failed to request EBS mount", "err", err)
			return rollback(err)
		}

		var ebsMountResponse types.EBSMountResponse
		if err := json.Unmarshal(reply.Data, &ebsMountResponse); err != nil {
			slog.Error("Failed to unmarshal volume response:", "err", err)
			return rollback(err)
		}

		if ebsMountResponse.Error != "" {
			slog.Error("Failed to mount volume", "error", ebsMountResponse.Error)
			return rollback(fmt.Errorf("failed to mount volume: %s", ebsMountResponse.Error))
		}

		slog.Debug("Mounted volume successfully", "response", ebsMountResponse.URI)
		instance.EBSRequests.Requests[k].NBDURI = ebsMountResponse.URI
		mounted = append(mounted, k)
	}

	return nil
}

func (a *volumeMounterAdapter) Unmount(instance *vm.VM) error {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()

	for _, ebsRequest := range instance.EBSRequests.Requests {
		ebsUnMountRequest, err := json.Marshal(ebsRequest)
		if err != nil {
			slog.Error("Failed to marshal volume payload for unmount", "err", err)
			continue
		}

		// ebs.unmount seals the block map to predastore. Teardown tolerates a
		// failed seal (log + continue) so terminate stays idempotent, but the
		// volume must NOT then go available: a reattach on a node without the
		// local WAL would find no checkpoint (bad superblock). On terminate the
		// volume is deleted regardless; on stop it stays attached/retryable.
		sealed := true
		msg, err := a.nc.Request(a.topic("unmount"), ebsUnMountRequest, unmountSealTimeout)
		if err != nil {
			slog.Error("Failed to unmount volume",
				"name", ebsRequest.Name, "instance", instance.ID, "err", err)
			sealed = false
		} else if sealErr := unmountResponseError(msg.Data); sealErr != nil {
			slog.Error("Volume unmount seal failed, leaving volume non-available",
				"instance", instance.ID, "volume", ebsRequest.Name, "err", sealErr)
			sealed = false
		} else {
			slog.Info("Unmounted volume",
				"instance", instance.ID, "volume", ebsRequest.Name, "data", string(msg.Data))
		}

		if sealed && !ebsRequest.EFI && a.volState != nil {
			if err := a.volState.UpdateVolumeState(ebsRequest.Name, "available", "", ""); err != nil {
				slog.Error("Failed to update volume state to available after unmount",
					"volumeId", ebsRequest.Name, "err", err)
			}
		}
	}

	return nil
}

// MountOne sends ebs.mount for a single request and writes the resolved
// NBDURI back into req.NBDURI. Used by hot-attach (Manager.AttachVolume).
func (a *volumeMounterAdapter) MountOne(req *types.EBSRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal ebs.mount request: %w", err)
	}

	reply, err := a.nc.Request(a.topic("mount"), payload, 30*time.Second)
	if err != nil {
		return fmt.Errorf("ebs.mount NATS request: %w", err)
	}

	var resp types.EBSMountResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return fmt.Errorf("unmarshal ebs.mount response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("ebs.mount returned error: %s", resp.Error)
	}
	if resp.URI == "" {
		return vm.ErrMountAmbiguous
	}

	req.NBDURI = resp.URI
	return nil
}

// UnmountOne sends ebs.unmount and returns any error. The handler seals the
// volume's block map to predastore, so the caller decides whether a failure
// blocks the volume's available transition.
func (a *volumeMounterAdapter) UnmountOne(req types.EBSRequest) error {
	if err := a.unmountOne(req); err != nil {
		slog.Error("UnmountOne failed", "volume", req.Name, "err", err)
		return err
	}
	slog.Info("UnmountOne: volume unmounted successfully", "volume", req.Name)
	return nil
}

// unmountOne sends ebs.unmount and returns any error.
func (a *volumeMounterAdapter) unmountOne(req types.EBSRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal unmount request: %w", err)
	}
	msg, err := a.nc.Request(a.topic("unmount"), payload, unmountSealTimeout)
	if err != nil {
		return fmt.Errorf("ebs.unmount NATS request: %w", err)
	}
	return unmountResponseError(msg.Data)
}

// unmountResponseError reports a seal/unmount failure from an ebs.unmount
// response payload: a non-empty Error, or a volume still reported mounted.
// Returns nil when the unmount and its block-map seal succeeded.
func unmountResponseError(data []byte) error {
	var resp types.EBSUnMountResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("unmarshal unmount response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("ebs.unmount returned error: %s", resp.Error)
	}
	if resp.Mounted {
		return fmt.Errorf("volume still mounted after unmount")
	}
	return nil
}

// instanceTypeResolverAdapter satisfies vm.InstanceTypeResolver by projecting
// EC2 instance type info into the SDK-agnostic vm.InstanceTypeSpec.
type instanceTypeResolverAdapter struct {
	rm *ResourceManager
}

var _ vm.InstanceTypeResolver = (*instanceTypeResolverAdapter)(nil)

func newInstanceTypeResolverAdapter(rm *ResourceManager) *instanceTypeResolverAdapter {
	return &instanceTypeResolverAdapter{rm: rm}
}

func (a *instanceTypeResolverAdapter) Resolve(name string) (vm.InstanceTypeSpec, bool) {
	it := a.rm.instanceTypes[name]
	if it == nil {
		return vm.InstanceTypeSpec{}, false
	}
	architecture := "x86_64"
	if it.ProcessorInfo != nil && len(it.ProcessorInfo.SupportedArchitectures) > 0 && it.ProcessorInfo.SupportedArchitectures[0] != nil {
		architecture = *it.ProcessorInfo.SupportedArchitectures[0]
	}
	return vm.InstanceTypeSpec{
		VCPUs:        int(instanceTypeVCPUs(it)),
		MemoryMiB:    int(instanceTypeMemoryMiB(it)),
		Architecture: architecture,
	}, true
}

// resourceControllerAdapter satisfies vm.ResourceController.
type resourceControllerAdapter struct {
	rm *ResourceManager
}

var _ vm.ResourceController = (*resourceControllerAdapter)(nil)

func newResourceControllerAdapter(rm *ResourceManager) *resourceControllerAdapter {
	return &resourceControllerAdapter{rm: rm}
}

func (a *resourceControllerAdapter) Allocate(instanceType string) error {
	it := a.rm.instanceTypes[instanceType]
	if it == nil {
		return fmt.Errorf("instance type %s not found", instanceType)
	}
	return a.rm.allocate(it)
}

func (a *resourceControllerAdapter) Deallocate(instanceType string) {
	it := a.rm.instanceTypes[instanceType]
	if it == nil {
		return
	}
	a.rm.deallocate(it)
}

func (a *resourceControllerAdapter) ReleaseToReservation(reservationID, instanceType string) {
	it := a.rm.instanceTypes[instanceType]
	if it == nil {
		return
	}
	a.rm.ReleaseToReservation(reservationID, it)
}

func (a *resourceControllerAdapter) CanAllocate(instanceType string, count int) int {
	it := a.rm.instanceTypes[instanceType]
	if it == nil {
		return 0
	}
	return a.rm.canAllocate(it, count)
}

// onInstanceRecoveringHook subscribes ec2.cmd.<id> early during Restore so
// terminate commands can land before the relaunch completes.
func (d *Daemon) onInstanceRecoveringHook() func(*vm.VM) {
	return func(instance *vm.VM) {
		d.mu.Lock()
		defer d.mu.Unlock()

		if _, ok := d.natsSubscriptions[instance.ID]; ok {
			return
		}
		sub, err := d.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instance.ID), d.handleEC2Events)
		if err != nil {
			slog.Error("OnInstanceRecovering: failed to early-subscribe per-instance topic",
				"instanceId", instance.ID, "err", err)
			return
		}
		d.natsSubscriptions[instance.ID] = sub
	}
}

// consumeCleanShutdownMarker returns the ConsumeCleanShutdownMarker callback.
// Returns true and deletes the marker when a clean shutdown was recorded.
func (d *Daemon) consumeCleanShutdownMarker() func() bool {
	return func() bool {
		if d.jsManager == nil {
			return false
		}
		marker, err := d.jsManager.ReadShutdownMarker(d.node)
		if err != nil || !marker {
			return false
		}
		slog.Info("Clean shutdown marker found, trusting KV state")
		_ = d.jsManager.DeleteShutdownMarker(d.node)
		return true
	}
}

// onInstanceUpHook subscribes per-instance NATS topics after Pending→Running.
// Returns the first subscribe error so the manager can roll back QMP.
func (d *Daemon) onInstanceUpHook() func(*vm.VM) error {
	return func(instance *vm.VM) error {
		d.mu.Lock()
		defer d.mu.Unlock()

		if existing, ok := d.natsSubscriptions[instance.ID]; ok {
			_ = existing.Unsubscribe()
		}
		consoleSubKey := instance.ID + ".console"
		if existing, ok := d.natsSubscriptions[consoleSubKey]; ok {
			_ = existing.Unsubscribe()
		}

		sub, err := d.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instance.ID), d.handleEC2Events)
		if err != nil {
			slog.Error("OnInstanceUp: failed to subscribe to per-instance topic",
				"instanceId", instance.ID, "err", err)
			return fmt.Errorf("subscribe ec2.cmd.%s: %w", instance.ID, err)
		}
		d.natsSubscriptions[instance.ID] = sub

		consoleSub, err := d.natsConn.Subscribe(
			fmt.Sprintf("ec2.%s.GetConsoleOutput", instance.ID),
			d.handleEC2GetConsoleOutput,
		)
		if err != nil {
			// Roll back the first sub so the instance doesn't end up Running with
			// one of two per-instance topics live — leaving GetConsoleOutput
			// unreachable while Stop / Terminate continue to work.
			if unsubErr := sub.Unsubscribe(); unsubErr != nil {
				slog.Warn("OnInstanceUp: failed to unsubscribe command topic during rollback",
					"instanceId", instance.ID, "err", unsubErr)
			}
			delete(d.natsSubscriptions, instance.ID)
			slog.Error("OnInstanceUp: failed to subscribe to console output topic",
				"instanceId", instance.ID, "err", err)
			return fmt.Errorf("subscribe ec2.%s.GetConsoleOutput: %w", instance.ID, err)
		}
		d.natsSubscriptions[consoleSubKey] = consoleSub

		// Bind the per-instance terminate subscription for any system-managed VM
		// (ELBv2 load balancers, EKS K3s control-plane VMs). OnInstanceUp is the
		// one funnel every launch path crosses — local placement on the
		// coordinator node, the remote-launch handler, and the
		// reconnect-to-surviving-QEMU recovery after a daemon restart. Without it
		// system.TerminateInstance.{id} has no responder, so a cluster-wide
		// teardown invoked on another node cannot stop this VM and deletes its
		// still-attached ENI (InvalidNetworkInterface.InUse).
		if tags.IsSystemManaged(instance.ManagedBy) {
			if subErr := d.subscribeSystemTerminateLocked(instance.ID); subErr != nil {
				slog.Error("OnInstanceUp: failed to re-arm system terminate subscription",
					"instanceId", instance.ID, "err", subErr)
			}
		}

		// Re-claim GPU(s) after a daemon restart with a still-running QEMU
		// process: the manager's reconnect path fires OnInstanceUp without
		// going through the handler-side Claim, so the GPU pool would
		// otherwise treat the slot as free. ReclaimByAddress is a no-op
		// when the same instance already owns the slot, so the launch and
		// start-stopped paths (which Claim before Run) are unaffected.
		if d.gpuManager != nil {
			for _, att := range instance.GPUAttachments {
				if att.MdevPath != "" {
					if err := d.gpuManager.ReclaimByMdev(att.MdevPath, instance.ID); err != nil {
						slog.Warn("Failed to re-claim MIG instance on restart",
							"mdev", att.MdevPath, "instanceId", instance.ID, "err", err)
					}
				} else if att.PCIAddress != "" {
					if err := d.gpuManager.ReclaimByAddress(att.PCIAddress, instance.ID); err != nil {
						slog.Warn("Failed to re-claim GPU on instance up",
							"gpu", att.PCIAddress, "instanceId", instance.ID, "err", err)
					}
				}
			}
		}

		// Republish vpc.add-nat so vpcd re-establishes the dnat_and_snat
		// rule on host-reboot recovery. The OVN NB entry doesn't always
		// survive the reboot, and vpcd's reconcile loop only re-creates
		// EIP-allocated NATs (reconcile.go:391) — direct-add NATs from
		// LaunchInstance / LaunchSystemInstance are otherwise unrecoverable.
		// Idempotent: vpcd's handler deletes-then-adds (topology.go:1297),
		// so this is a no-op on fresh launches where the initial publish
		// already fired.
		//
		// instance.PublicIP only carries auto-assigned public IPs; an
		// associated Elastic IP lives in the EIP store, so resolve it from
		// there when the instance field is empty — otherwise a rebooted host
		// never re-announces the EIP's NAT and the VM loses connectivity.
		if d.natsConn != nil && instance.ENIId != "" && instance.Instance != nil {
			publicIP := instance.PublicIP
			if publicIP == "" && d.eipService != nil {
				if eipIP, ok := d.eipService.AssociatedPublicIPForInstance(instance.AccountID, instance.ID); ok {
					publicIP = eipIP
				}
			}
			vpcID := ""
			privateIP := ""
			if instance.Instance.VpcId != nil {
				vpcID = *instance.Instance.VpcId
			}
			if instance.Instance.PrivateIpAddress != nil {
				privateIP = *instance.Instance.PrivateIpAddress
			}
			if publicIP != "" && vpcID != "" && privateIP != "" {
				portName := topology.Port(instance.ENIId)
				utils.PublishNATEvent(d.natsConn, "vpc.add-nat", vpcID, publicIP, privateIP, portName, instance.ENIMac)
			}
		}
		return nil
	}
}

// onInstanceDownHook returns the daemon's OnInstanceDown callback. It
// unsubscribes the per-instance NATS topics that onInstanceUpHook
// registered.
func (d *Daemon) onInstanceDownHook() func(string) {
	return func(instanceID string) {
		d.mu.Lock()
		defer d.mu.Unlock()

		if sub, ok := d.natsSubscriptions[instanceID]; ok {
			if err := sub.Unsubscribe(); err != nil {
				slog.Error("OnInstanceDown: failed to unsubscribe instance topic",
					"instanceId", instanceID, "err", err)
			}
			delete(d.natsSubscriptions, instanceID)
		}
		consoleSubKey := instanceID + ".console"
		if sub, ok := d.natsSubscriptions[consoleSubKey]; ok {
			if err := sub.Unsubscribe(); err != nil {
				slog.Error("OnInstanceDown: failed to unsubscribe console topic",
					"instanceId", instanceID, "err", err)
			}
			delete(d.natsSubscriptions, consoleSubKey)
		}
		// Drop the system terminate subscription (system VMs only; a no-op for
		// regular instances that never registered one).
		terminateSubKey := fmt.Sprintf("system.TerminateInstance.%s", instanceID)
		if sub, ok := d.natsSubscriptions[terminateSubKey]; ok {
			if err := sub.Unsubscribe(); err != nil {
				slog.Error("OnInstanceDown: failed to unsubscribe system terminate topic",
					"instanceId", instanceID, "err", err)
			}
			delete(d.natsSubscriptions, terminateSubKey)
		}
	}
}

// buildVMManagerDeps assembles the vm.Deps struct for the running daemon.
// All collaborators must already be initialized; callers are expected to
// invoke this from Daemon.Start after services and JetStream are ready.
func (d *Daemon) buildVMManagerDeps() vm.Deps {
	return vm.Deps{
		NodeID:             d.node,
		StateStore:         d.stateStore,
		VolumeMounter:      newVolumeMounterAdapter(d.natsConn, d.node, d.volumeService),
		NetworkPlumber:     d.networkPlumber,
		InstanceTypes:      newInstanceTypeResolverAdapter(d.resourceMgr),
		Resources:          newResourceControllerAdapter(d.resourceMgr),
		VolumeStateUpdater: d.volumeService,
		InstanceCleaner:    newInstanceCleanerAdapter(d),
		Hooks: vm.ManagerHooks{
			OnInstanceUp:           d.onInstanceUpHook(),
			OnInstanceDown:         d.onInstanceDownHook(),
			OnInstanceRecovering:   d.onInstanceRecoveringHook(),
			BeforeInstanceRelaunch: d.refreshSystemInstanceState,
		},
		ShutdownSignal:             d.shuttingDown.Load,
		CrashHandler:               d.vmMgr.HandleCrash,
		TransitionState:            d.TransitionState,
		DevNetworking:              d.config.Daemon.DevNetworking,
		BindHost:                   d.config.Host,
		DetachDelay:                d.detachDelay,
		ConsumeCleanShutdownMarker: d.consumeCleanShutdownMarker(),
	}
}

// instanceCleanerAdapter satisfies vm.InstanceCleaner by delegating to the
// daemon's existing volume/VPC/EIP/placement-group services. The manager
// owns the QMP and tap teardown directly; this adapter covers the
// AWS-resource cleanup steps that require service access.
type instanceCleanerAdapter struct {
	d *Daemon
}

var _ vm.InstanceCleaner = (*instanceCleanerAdapter)(nil)

func newInstanceCleanerAdapter(d *Daemon) *instanceCleanerAdapter {
	return &instanceCleanerAdapter{d: d}
}

// DeleteVolumes deletes EFI internal volumes via ebs.delete and user volumes
// flagged DeleteOnTermination via the volume service.
// Errors are logged per volume; partial failure is tolerated.
func (a *instanceCleanerAdapter) DeleteVolumes(instance *vm.VM) error {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()

	var firstErr error
	for _, ebsRequest := range instance.EBSRequests.Requests {
		// Internal volumes (EFI) always go through ebs.delete to stop their
		// viperblockd processes. S3 data is cleaned up via the parent root
		// volume's DeleteVolume (which removes the -efi/ prefix).
		if ebsRequest.EFI {
			ebsDeleteData, err := json.Marshal(types.EBSDeleteRequest{Volume: ebsRequest.Name})
			if err != nil {
				slog.Error("Failed to marshal ebs.delete request for internal volume",
					"name", ebsRequest.Name, "err", err)
				firstErr = cmp.Or(firstErr, err)
				continue
			}
			deleteMsg, err := a.d.natsConn.Request("ebs.delete", ebsDeleteData, 30*time.Second)
			if err != nil {
				slog.Warn("Failed to send ebs.delete for internal volume",
					"name", ebsRequest.Name, "id", instance.ID, "err", err)
				firstErr = cmp.Or(firstErr, err)
			} else {
				slog.Info("Sent ebs.delete for internal volume",
					"name", ebsRequest.Name, "id", instance.ID, "data", string(deleteMsg.Data))
			}
			continue
		}

		// User-visible volumes: only delete when DeleteOnTermination is set.
		if !ebsRequest.DeleteOnTermination {
			slog.Info("Volume has DeleteOnTermination=false, skipping deletion",
				"name", ebsRequest.Name, "id", instance.ID)
			continue
		}

		slog.Info("Deleting volume with DeleteOnTermination=true",
			"name", ebsRequest.Name, "id", instance.ID)
		if a.d.volumeService == nil {
			slog.Warn("Volume service not configured, cannot delete volume",
				"name", ebsRequest.Name, "id", instance.ID)
			continue
		}
		if _, err := a.d.volumeService.DeleteVolume(&ec2.DeleteVolumeInput{
			VolumeId: &ebsRequest.Name,
		}, instance.AccountID); err != nil && !awserrors.IsNotFound(err) {
			slog.Error("Failed to delete volume on termination",
				"name", ebsRequest.Name, "id", instance.ID, "err", err)
			firstErr = cmp.Or(firstErr, err)
		} else {
			slog.Info("Deleted volume on termination",
				"name", ebsRequest.Name, "id", instance.ID)
		}
	}
	return firstErr
}

// CleanupMgmtNetwork tears down the management TAP device (derived from
// instance.ID so unsetup instances are tolerated) and releases the
// management IP allocation if the daemon has one.
func (a *instanceCleanerAdapter) CleanupMgmtNetwork(instance *vm.VM) {
	mgmtTap := vm.MgmtTapName(instance.ID)
	if err := a.d.networkPlumber.CleanupTap(mgmtTap); err != nil {
		slog.Warn("Failed to clean up mgmt tap device",
			"tap", mgmtTap, "instanceId", instance.ID, "err", err)
	}
	if a.d.mgmtIPAllocator != nil {
		a.d.mgmtIPAllocator.Release(instance.ID)
	}
}

// ReleasePublicIP publishes vpc.delete-nat for the OVN dnat_and_snat rule
// and releases the public IP back to the external IPAM pool. No-op when
// the instance has no public IP.
func (a *instanceCleanerAdapter) ReleasePublicIP(instance *vm.VM) error {
	if instance.PublicIP == "" || instance.PublicIPPool == "" || a.d.externalIPAM == nil {
		return nil
	}

	portName := topology.Port(instance.ENIId)
	vpcId := ""
	logicalIP := ""
	if instance.Instance != nil {
		if instance.Instance.VpcId != nil {
			vpcId = *instance.Instance.VpcId
		}
		if instance.Instance.PrivateIpAddress != nil {
			logicalIP = *instance.Instance.PrivateIpAddress
		}
	}
	utils.PublishNATEvent(a.d.natsConn, "vpc.delete-nat", vpcId, instance.PublicIP, logicalIP, portName, "")

	if err := a.d.externalIPAM.ReleaseIP(instance.PublicIPPool, instance.PublicIP, instance.ENIId); err != nil {
		slog.Warn("Failed to release public IP on termination",
			"ip", instance.PublicIP, "pool", instance.PublicIPPool, "err", err)
		return err
	}
	slog.Info("Released public IP on termination",
		"ip", instance.PublicIP, "instanceId", instance.ID)
	return nil
}

// DetachAndDeleteENI detaches the auto-created ENI from the instance and
// deletes it via the VPC service. NotFound is tolerated. Extra ENIs are
// cleaned up by the load-balancer service via its own teardown loop and
// are not touched here.
func (a *instanceCleanerAdapter) DetachAndDeleteENI(instance *vm.VM) error {
	if instance.ENIId == "" || a.d.vpcService == nil {
		return nil
	}
	// Best-effort detach; a failure here must not block deletion — the force
	// delete below bypasses the in-use guard for the owning instance, breaking
	// the un-terminable-ENI deadlock (ADR-0003 §2).
	if detachErr := a.d.vpcService.DetachENI(instance.AccountID, instance.ENIId); detachErr != nil {
		slog.Warn("Failed to detach ENI on termination",
			"eni", instance.ENIId, "instanceId", instance.ID, "err", detachErr)
	}
	if err := a.d.vpcService.ForceDeleteInstanceENI(instance.AccountID, instance.ENIId); err != nil {
		slog.Error("Failed to delete ENI on termination",
			"eni", instance.ENIId, "instanceId", instance.ID, "err", err)
		return err
	}
	slog.Info("Deleted ENI on termination",
		"eni", instance.ENIId, "instanceId", instance.ID)
	return nil
}

// RemoveFromPlacementGroup unbinds the instance from its placement group
// if one is set. No-op for ungrouped instances and when the placement
// group service is not configured.
func (a *instanceCleanerAdapter) RemoveFromPlacementGroup(instance *vm.VM) error {
	if instance.PlacementGroupName == "" || a.d.placementGroupService == nil {
		return nil
	}
	if _, err := a.d.placementGroupService.RemoveInstance(&handlers_ec2_placementgroup.RemoveInstanceInput{
		GroupName:  instance.PlacementGroupName,
		NodeName:   instance.PlacementGroupNode,
		InstanceID: instance.ID,
	}, instance.AccountID); err != nil {
		slog.Error("Failed to remove instance from placement group",
			"instanceId", instance.ID, "groupName", instance.PlacementGroupName, "err", err)
		return err
	}
	return nil
}

// ReleaseGPU unbinds the instance's GPU from vfio-pci and rebinds to its
// original host driver. No-op for instances without a GPU allocation or
// when GPU passthrough is disabled.
func (a *instanceCleanerAdapter) ReleaseGPU(instance *vm.VM) error {
	if a.d.gpuManager == nil || len(instance.GPUAttachments) == 0 {
		return nil
	}
	if err := a.d.gpuManager.Release(instance.ID); err != nil {
		slog.Error("Failed to release GPU on stop, device may need manual rebind",
			"gpus", instance.GPUAttachments, "instanceId", instance.ID, "err", err)
		return err
	}
	slog.Info("GPU released", "gpus", instance.GPUAttachments, "instanceId", instance.ID)
	return nil
}
