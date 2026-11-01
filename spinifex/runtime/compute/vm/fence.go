package vm

import (
	"context"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// fenceStateReasonCode is the StateReason a fenced instance carries. Distinct
// from Server.RecoveryFailed so an operator can tell "this node could not
// recover the guest" from "another node owns this guest's disks now".
const fenceStateReasonCode = "Server.VolumeFenced"

// VolumeFenced reports whether this is the record of an instance a fence
// stopped. It reads the projection because that is where the fence writes the
// reason, and both the node's own VM and the cluster record carry one.
//
// It exists to keep a fenced instance out of every automatic relaunch. A fence
// deliberately does not seal, so the volumes it leaves behind are a state
// nothing has proven mountable, and relaunching is how that becomes permanent.
// Restarting one is an operator's decision made against the volumes.
func VolumeFenced(projection *ec2.Instance) bool {
	if projection == nil || projection.StateReason == nil {
		return false
	}
	return aws.StringValue(projection.StateReason.Code) == fenceStateReasonCode
}

// FenceVolume stops the guest running against volumeID on this node, because
// the volume's lease has moved and this node is no longer its owner.
//
// It is deliberately the narrowest teardown in the package. The guest process
// is killed and its taps are removed, and nothing else: the volumes, ENIs and
// addresses all belong to whichever node holds the instance now, so releasing
// them here would tear down resources in live use elsewhere. For the same
// reason the volumes are not unmounted, which would seal this node's stale copy
// over the winner's.
//
// Returns the instance it stopped, or "" when no local guest was using the
// volume — the ordinary case, since a volume is usually fenced after its
// instance has already gone.
func (m *Manager) FenceVolume(ctx context.Context, volumeID, reason string) string {
	instance := m.instanceUsingVolume(volumeID)
	if instance == nil {
		return ""
	}

	skip := false
	var observed InstanceState
	m.Inspect(instance, func(v *VM) {
		observed = v.Status
		if v.Status == StateError || v.Status == StateShuttingDown || v.Status == StateTerminated {
			skip = true
			return
		}
		if v.Instance != nil {
			v.Instance.StateReason = &ec2.StateReason{
				Code:    aws.String(fenceStateReasonCode),
				Message: aws.String(reason),
			}
		}
	})
	if skip {
		slog.InfoContext(ctx, "FenceVolume: instance already in a terminal state",
			"instanceId", instance.ID, "volumeId", volumeID, "status", string(observed))
		return instance.ID
	}

	if err := m.transitionWithPrecheck(instance, StateError); err != nil {
		slog.ErrorContext(ctx, "FenceVolume transition failed", "instanceId", instance.ID, "err", err)
		if m.Status(instance) != StateError {
			return instance.ID
		}
	}
	slog.ErrorContext(ctx, "Instance fenced: another node owns its volume, so the guest is being stopped here",
		"instanceId", instance.ID, "volumeId", volumeID, "reason", reason)

	m.goroutineWg.Go(func() {
		m.killFencedQEMU(ctx, instance)
		m.cleanupTapDevices(instance)
		m.Inspect(instance, func(v *VM) { v.LastNode = m.deps.NodeID })
		if err := m.writeRunningState(); err != nil {
			slog.ErrorContext(ctx, "FenceVolume: failed to persist state after fencing",
				"instanceId", instance.ID, "err", err)
		}
	})
	return instance.ID
}

// fencedQEMUKillTimeout bounds the wait for a SIGKILLed fenced guest to exit.
const fencedQEMUKillTimeout = 10 * time.Second

// killFencedQEMU ends a fenced guest without a powerdown. Its disk is gone, so a
// graceful shutdown has nothing to flush to, and while it waits the guest's taps
// and process outlive its ownership as a survivor starts it again.
func (m *Manager) killFencedQEMU(ctx context.Context, instance *VM) {
	pid, err := hostprocess.ReadPidFile(instance.ID)
	if err != nil || pid <= 0 {
		// No PID to kill, so ask QEMU itself; quit skips the guest's shutdown.
		if instance.QMPClient != nil {
			if _, qerr := sendQMPCommandWithTimeout(ctx, instance.QMPClient,
				qmp.QMPCommand{Execute: "quit"}, instance.ID, qmpCommandTimeout); qerr != nil {
				slog.WarnContext(ctx, "FenceVolume: QMP quit failed (VM may already be stopped)",
					"instanceId", instance.ID, "err", qerr)
			}
		}
		return
	}
	if !hostprocess.ProcessAlive(pid) {
		_ = hostprocess.RemovePidFile(instance.ID)
		return
	}
	if err := hostprocess.ForceKillProcess(pid, fencedQEMUKillTimeout); err != nil {
		slog.ErrorContext(ctx, "FenceVolume: failed to kill the fenced guest",
			"instanceId", instance.ID, "pid", pid, "err", err)
		return
	}
	_ = hostprocess.RemovePidFile(instance.ID)
}

// instanceUsingVolume returns the local instance holding volumeID, or nil. The
// map is this node's own running set, so a hit means the guest is here.
func (m *Manager) instanceUsingVolume(volumeID string) *VM {
	var found *VM
	m.View(func(vms map[string]*VM) {
		for _, v := range vms {
			for _, req := range v.EBSRequests.Requests {
				if req.Name == volumeID {
					found = v
					return
				}
			}
		}
	})
	return found
}
