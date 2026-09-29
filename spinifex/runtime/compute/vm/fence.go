package vm

import (
	"context"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// fenceStateReasonCode is the StateReason a fenced instance carries. Distinct
// from Server.RecoveryFailed so an operator can tell "this node could not
// recover the guest" from "another node owns this guest's disks now".
const fenceStateReasonCode = "Server.VolumeFenced"

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
		m.shutdownQEMU(instance)
		m.cleanupTapDevices(instance)
		m.Inspect(instance, func(v *VM) { v.LastNode = m.deps.NodeID })
		if err := m.writeRunningState(); err != nil {
			slog.ErrorContext(ctx, "FenceVolume: failed to persist state after fencing",
				"instanceId", instance.ID, "err", err)
		}
	})
	return instance.ID
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
