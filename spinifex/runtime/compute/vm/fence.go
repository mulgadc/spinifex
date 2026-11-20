package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"golang.org/x/sys/unix"
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
		m.dropUEFIWriter(instance.ID, "fence", true)
		m.cleanupTapDevices(instance)
		m.Inspect(instance, func(v *VM) { v.LastNode = m.deps.NodeID })
		if err := m.writeRunningState(); err != nil {
			slog.ErrorContext(ctx, "FenceVolume: failed to persist state after fencing",
				"instanceId", instance.ID, "err", err)
		}
	})
	return instance.ID
}

// ErrNotFencePaused reports a resume for a volume whose fence paused no guest
// here, so there is nothing a resume may continue.
var ErrNotFencePaused = errors.New("no guest here is paused for this volume's fence")

// fenceExpiryGrace is how long past the provider's window the daemon waits
// before it stops a paused guest itself, so the provider's stop normally wins.
const fenceExpiryGrace = 10 * time.Second

// PauseForFence pauses the guest running against volumeID, whose export was
// fenced at generation because this node could not confirm the volume's lease.
// The guest's process is pinned before it is paused; a guest that cannot be
// pinned or paused is stopped as by FenceVolume. Unless resumed, it is stopped
// pauseFor plus fenceExpiryGrace later whether or not the provider is heard from.
func (m *Manager) PauseForFence(ctx context.Context, volumeID, reason string, generation uint64, pauseFor time.Duration) string {
	instance := m.instanceUsingVolume(volumeID)
	if instance == nil {
		return ""
	}
	var running bool
	m.Inspect(instance, func(v *VM) { running = v.Status == StateRunning })
	if !running {
		return m.FenceVolume(ctx, volumeID, reason)
	}

	pidfd, pid, err := m.pinRunningQEMU(instance)
	if err != nil {
		slog.ErrorContext(ctx, "PauseForFence: could not pin the guest's process, stopping it instead",
			"instanceId", instance.ID, "volumeId", volumeID, "err", err)
		return m.FenceVolume(ctx, volumeID, reason)
	}
	if _, err := sendQMPCommandWithTimeout(ctx, instance.QMPClient,
		qmp.QMPCommand{Execute: "stop"}, instance.ID, qmpCommandTimeout); err != nil {
		_ = unix.Close(pidfd)
		slog.ErrorContext(ctx, "PauseForFence: could not pause the guest, stopping it instead",
			"instanceId", instance.ID, "volumeId", volumeID, "err", err)
		return m.FenceVolume(ctx, volumeID, reason)
	}

	m.Inspect(instance, func(v *VM) {
		if v.fencePaused == nil {
			v.fencePaused = make(map[string]uint64)
		}
		v.fencePaused[volumeID] = generation
		delete(v.fenceResumed, volumeID)
		if v.fencePidfd == 0 {
			v.fencePidfd, v.fencePid, pidfd = pidfd, pid, 0
		}
	})
	if pidfd != 0 {
		_ = unix.Close(pidfd)
	}
	if pauseFor > 0 {
		grace := fenceExpiryGrace
		if m.fenceGrace != 0 {
			grace = m.fenceGrace
		}
		time.AfterFunc(pauseFor+grace, func() {
			m.StopFencePaused(context.WithoutCancel(ctx), volumeID, generation, "the pause window ended")
		})
	}
	slog.WarnContext(ctx, "Instance paused: this node could not confirm its volume's lease, and is trying to get it back",
		"instanceId", instance.ID, "volumeId", volumeID, "generation", generation, "reason", reason)
	return instance.ID
}

// pinRunningQEMU opens a pidfd on the QEMU this manager started for instance,
// refusing a PID file that names any other process.
func (m *Manager) pinRunningQEMU(instance *VM) (int, int, error) {
	pid, err := hostprocess.ReadPidFile(instance.ID)
	if err != nil || pid <= 0 {
		return 0, 0, fmt.Errorf("read the guest's PID file: %w", err)
	}
	if owned := instance.qemuPID.Load(); owned != 0 && int(owned) != pid {
		return 0, 0, fmt.Errorf("the PID file names %d, not the QEMU this manager started (%d)", pid, owned)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("pidfd_open %d: %w", pid, err)
	}
	return fd, pid, nil
}

// StopFencePaused stops the guest paused for volumeID's fence at generation,
// and does nothing to a guest that is not, so a stale or repeated stop can
// never reach a successor. Returns the instance stopped, or "".
func (m *Manager) StopFencePaused(ctx context.Context, volumeID string, generation uint64, reason string) string {
	instance := m.instanceUsingVolume(volumeID)
	if instance == nil {
		return ""
	}
	var paused bool
	m.Inspect(instance, func(v *VM) {
		g, ok := v.fencePaused[volumeID]
		paused = ok && g == generation
	})
	if !paused {
		slog.InfoContext(ctx, "volume fence: ignoring a stop for a fence this guest is not paused under",
			"instanceId", instance.ID, "volumeId", volumeID, "generation", generation)
		return ""
	}
	return m.FenceVolume(ctx, volumeID, reason)
}

// ResumeAfterFence continues a guest that PauseForFence paused at generation,
// once volumeID is exported again under a lease this node holds. The guest
// stays paused while any other of its volumes is still fenced, and resumed
// reports whether this call continued it. before runs just ahead of the
// continue; if it or the continue fails the guest is paused for this fence
// again. A repeat for a fence already resumed succeeds without acting.
func (m *Manager) ResumeAfterFence(ctx context.Context, volumeID string, generation uint64, before func(instanceID string) error) (string, bool, error) {
	instance := m.instanceUsingVolume(volumeID)
	if instance == nil {
		return "", false, ErrNotFencePaused
	}
	var paused, repeat bool
	var remaining, pidfd int
	m.Inspect(instance, func(v *VM) {
		if g, ok := v.fenceResumed[volumeID]; ok && g == generation {
			repeat = true
			return
		}
		g, ok := v.fencePaused[volumeID]
		paused = v.Status == StateRunning && ok && g == generation
		if paused {
			delete(v.fencePaused, volumeID)
			if v.fenceResumed == nil {
				v.fenceResumed = make(map[string]uint64)
			}
			v.fenceResumed[volumeID] = generation
		}
		remaining, pidfd = len(v.fencePaused), v.fencePidfd
	})
	if repeat {
		return instance.ID, false, nil
	}
	if !paused {
		return instance.ID, false, ErrNotFencePaused
	}
	if remaining > 0 {
		slog.InfoContext(ctx, "ResumeAfterFence: volume restored, guest stays paused for its other volumes",
			"instanceId", instance.ID, "volumeId", volumeID, "still_fenced", remaining)
		return instance.ID, false, nil
	}
	repause := func(err error) (string, bool, error) {
		m.Inspect(instance, func(v *VM) {
			delete(v.fenceResumed, volumeID)
			if v.fencePaused == nil {
				v.fencePaused = make(map[string]uint64)
			}
			v.fencePaused[volumeID] = generation
		})
		return instance.ID, false, err
	}
	if pidfd != 0 {
		if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
			return repause(fmt.Errorf("the paused guest's process is gone: %w", err))
		}
	}
	if before != nil {
		if err := before(instance.ID); err != nil {
			return repause(err)
		}
	}
	if _, err := sendQMPCommandWithTimeout(ctx, instance.QMPClient,
		qmp.QMPCommand{Execute: "cont"}, instance.ID, qmpCommandTimeout); err != nil {
		return repause(fmt.Errorf("resume the guest: %w", err))
	}
	m.Inspect(instance, func(v *VM) {
		if v.fencePidfd != 0 {
			_ = unix.Close(v.fencePidfd)
		}
		v.fencePidfd, v.fencePid = 0, 0
	})
	slog.WarnContext(ctx, "Instance resumed: its node got the volume's lease back",
		"instanceId", instance.ID, "volumeId", volumeID, "generation", generation)
	return instance.ID, true, nil
}

// fencedQEMUKillTimeout bounds the wait for a SIGKILLed fenced guest to exit.
const fencedQEMUKillTimeout = 10 * time.Second

// killFencedQEMU ends a fenced guest without a powerdown. Its disk is gone, so a
// graceful shutdown has nothing to flush to, and while it waits the guest's taps
// and process outlive its ownership as a survivor starts it again.
func (m *Manager) killFencedQEMU(ctx context.Context, instance *VM) {
	var pidfd, pausedPid int
	m.Inspect(instance, func(v *VM) {
		pidfd, pausedPid = v.fencePidfd, v.fencePid
		v.fencePaused, v.fenceResumed, v.fencePidfd, v.fencePid = nil, nil, 0, 0
	})
	if pidfd != 0 {
		m.killPinnedQEMU(ctx, instance, pidfd, pausedPid)
		return
	}
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
	if hostprocess.ProcessExited(pid) {
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

// killPinnedQEMU kills a fence-paused guest through the pidfd taken when it was
// paused, so a pid reused after it exited can never be signalled. The PID file
// is removed only while it still names that process.
func (m *Manager) killPinnedQEMU(ctx context.Context, instance *VM, pidfd, pid int) {
	defer func() { _ = unix.Close(pidfd) }()
	if err := unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		slog.ErrorContext(ctx, "FenceVolume: failed to kill the paused guest",
			"instanceId", instance.ID, "pid", pid, "err", err)
		return
	}
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}} //nolint:gosec // a pidfd is a small non-negative descriptor.
	if _, err := unix.Poll(fds, int(fencedQEMUKillTimeout.Milliseconds())); err != nil {
		slog.WarnContext(ctx, "FenceVolume: could not wait for the paused guest to exit",
			"instanceId", instance.ID, "pid", pid, "err", err)
	}
	if current, err := hostprocess.ReadPidFile(instance.ID); err == nil && current == pid {
		_ = hostprocess.RemovePidFile(instance.ID)
	}
}
