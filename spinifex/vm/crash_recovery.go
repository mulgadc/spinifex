package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	// MaxRestartsInWindow is the cap on automatic crash-restarts inside
	// RestartWindow before the manager leaves the instance in StateError.
	MaxRestartsInWindow = 3
	// RestartWindow is the rolling window over which crashes count toward
	// MaxRestartsInWindow. Crashes older than this reset the counter.
	RestartWindow      = 10 * time.Minute
	restartBackoffBase = 5 * time.Second
	restartBackoffMax  = 2 * time.Minute
)

// restartAfterFunc is a var so tests can capture the restart callback without
// firing a real timer (backoff is multi-second).
var restartAfterFunc = time.AfterFunc

// RestartBackoff computes the exponential backoff delay for the given
// restart count. Pure function — no side effects.
func RestartBackoff(restartCount int) time.Duration {
	delay := restartBackoffBase
	for range restartCount {
		delay *= 2
		if delay > restartBackoffMax {
			return restartBackoffMax
		}
	}
	return delay
}

// ClassifyCrashReason extracts a human-readable crash reason from cmd.Wait()'s
// error, distinguishing OOM kills, segfaults, aborts, and other signals.
func ClassifyCrashReason(waitErr error) string {
	if waitErr == nil {
		return "clean-exit"
	}

	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return "unknown"
	}

	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return "unknown"
	}

	if status.Signaled() {
		switch status.Signal() {
		case syscall.SIGKILL:
			return "oom-killed"
		case syscall.SIGSEGV:
			return "segfault"
		case syscall.SIGABRT:
			return "abort"
		default:
			return fmt.Sprintf("signal-%d", status.Signal())
		}
	}

	if status.Exited() {
		return fmt.Sprintf("exit-%d", status.ExitStatus())
	}

	return "unknown"
}

// HandleCrash reacts to an unexpected QEMU exit after startup was confirmed:
// classifies the reason, transitions to error, releases resources, unmounts
// volumes, and calls MaybeRestart. Wired as Deps.CrashHandler.
func (m *Manager) HandleCrash(instance *VM, waitErr error) {
	if status := m.Status(instance); status != StateRunning {
		slog.Debug("QEMU exited but instance not in running state, skipping crash handler",
			"instance", instance.ID, "status", status)
		return
	}

	if m.deps.ShutdownSignal != nil && m.deps.ShutdownSignal() {
		slog.Debug("QEMU exited during coordinated shutdown, skipping crash handler",
			"instance", instance.ID)
		return
	}

	reason := ClassifyCrashReason(waitErr)
	slog.Error("VM process crashed", "instance", instance.ID, "reason", reason, "err", waitErr)

	if m.deps.TransitionState != nil {
		if err := m.deps.TransitionState(instance, StateError); err != nil {
			slog.Error("Failed to transition crashed instance to error state",
				"instance", instance.ID, "err", err)
		}
	}

	now := time.Now()
	m.UpdateState(instance.ID, func(v *VM) {
		v.Health.CrashCount++
		v.Health.LastCrashTime = now
		v.Health.LastCrashReason = reason
		if v.Health.FirstCrashTime.IsZero() {
			v.Health.FirstCrashTime = now
		}
	})

	if m.deps.Resources != nil && instance.InstanceType != "" {
		slog.Info("Deallocating resources for crashed instance",
			"instance", instance.ID, "type", instance.InstanceType)
		m.deallocateResources(instance)
	}

	// The slot just returned to the reservation (deallocateResources above), but
	// the restart re-allocates from the general pool, so drop the binding under the
	// manager lock. This keeps the crash release and restart allocation on the same
	// pool; leaving it set would drift the shared reservation counter on the later
	// stop/terminate while other instances still consume the reservation.
	if instance.CapacityReservationId != "" {
		m.UpdateState(instance.ID, func(v *VM) { v.CapacityReservationId = "" })
	}

	if instance.Config.QMPSocket != "" {
		_ = os.Remove(instance.Config.QMPSocket)
	}
	removeTelemetryArtifacts(instance)

	if m.deps.VolumeMounter != nil {
		if err := m.deps.VolumeMounter.Unmount(instance); err != nil {
			slog.Error("Volume unmount failed during crash handling",
				"instance", instance.ID, "err", err)
		}
	}

	if err := m.writeRunningState(); err != nil {
		slog.Error("Failed to persist state after crash handling",
			"instance", instance.ID, "err", err)
	}

	m.MaybeRestart(instance)
}

// MaybeRestart checks restart policy and schedules a restart via time.AfterFunc.
// No-op on shutdown, unknown instance type, exceeded restart window, or no capacity.
func (m *Manager) MaybeRestart(instance *VM) {
	if m.deps.ShutdownSignal != nil && m.deps.ShutdownSignal() {
		slog.Info("Skipping restart during shutdown", "instance", instance.ID)
		return
	}

	now := time.Now()

	var (
		crashCount   int
		restartCount int
		exceeded     bool
	)
	m.UpdateState(instance.ID, func(v *VM) {
		health := &v.Health
		if !health.FirstCrashTime.IsZero() && now.Sub(health.FirstCrashTime) > RestartWindow {
			slog.Info("Crash window expired, resetting counters", "instance", v.ID)
			health.CrashCount = 1
			health.FirstCrashTime = now
			health.RestartCount = 0
		}
		if health.CrashCount > MaxRestartsInWindow {
			exceeded = true
			crashCount = health.CrashCount
			return
		}
		restartCount = health.RestartCount
	})
	if exceeded {
		slog.Error("Instance exceeded max restarts in window, leaving in error state",
			"instance", instance.ID,
			"crashes", crashCount,
			"window", RestartWindow,
			"max", MaxRestartsInWindow)
		m.releaseGPUOnTerminalCrash(instance)
		return
	}

	if m.deps.InstanceTypes != nil {
		if _, ok := m.deps.InstanceTypes.Resolve(instance.InstanceType); !ok {
			slog.Error("Unknown instance type, cannot restart",
				"instance", instance.ID, "type", instance.InstanceType)
			m.releaseGPUOnTerminalCrash(instance)
			return
		}
	}

	if m.deps.Resources != nil {
		if m.deps.Resources.CanAllocate(instance.InstanceType, 1) < 1 {
			slog.Error("Insufficient resources to restart instance",
				"instance", instance.ID, "type", instance.InstanceType)
			m.releaseGPUOnTerminalCrash(instance)
			return
		}
	}

	delay := RestartBackoff(restartCount)

	slog.Info("Scheduling instance restart",
		"instance", instance.ID,
		"delay", delay,
		"restartCount", restartCount+1)

	restartAfterFunc(delay, func() {
		m.RestartCrashedInstance(instance)
	})
}

// RestartCrashedInstance re-verifies the instance is in error state, reallocates
// resources (HandleCrash deallocated them), and relaunches via Manager.Run.
// Rolls back the transition and allocation if relaunch fails.
func (m *Manager) RestartCrashedInstance(instance *VM) {
	var skipReason string
	var restartCount int
	m.UpdateState(instance.ID, func(v *VM) {
		if v.Status != StateError {
			skipReason = fmt.Sprintf("not in error state (%s)", v.Status)
			return
		}
		if m.deps.ShutdownSignal != nil && m.deps.ShutdownSignal() {
			skipReason = "shutting down"
			return
		}
		v.Health.RestartCount++
		restartCount = v.Health.RestartCount
		// Refresh LaunchTime so the pending watchdog gives the relaunch a fresh
		// window; the stale original time otherwise trips launch_timeout on the
		// next tick and terminates the crash-restart mid-flight (cf. restore.go).
		if v.Instance != nil {
			now := time.Now()
			v.Instance.LaunchTime = &now
		}
	})
	if skipReason != "" {
		slog.Info("Skipping restart of crashed instance",
			"instance", instance.ID, "reason", skipReason)
		return
	}

	slog.Info("Restarting crashed instance",
		"instance", instance.ID,
		"restartCount", restartCount)

	if m.deps.Resources == nil {
		slog.Error("ResourceController not wired, cannot restart crashed instance",
			"instance", instance.ID)
		return
	}
	if err := m.deps.Resources.Allocate(instance.InstanceType); err != nil {
		slog.Error("Insufficient resources to restart crashed instance",
			"instance", instance.ID, "type", instance.InstanceType, "err", err)
		return
	}

	if m.deps.TransitionState != nil {
		if err := m.deps.TransitionState(instance, StatePending); err != nil {
			slog.Error("Failed to transition instance to pending for restart",
				"instance", instance.ID, "err", err)
			m.deallocateResources(instance)
			return
		}
	}

	// Crash recovery is a background path with no request context.
	if err := m.Run(context.Background(), instance); err != nil {
		slog.Error("Failed to restart crashed instance",
			"instance", instance.ID, "err", err)
		m.deps.Resources.Deallocate(instance.InstanceType)
		if m.deps.TransitionState != nil {
			if err := m.deps.TransitionState(instance, StateError); err != nil {
				slog.Error("Failed to transition instance back to error after restart failure",
					"instance", instance.ID, "err", err)
			}
		}
		// The relaunch attempt failed, so the instance settles in StateError
		// for good — release the GPU slot it still holds from before the
		// crash rather than leaving it stuck until the daemon restarts.
		m.releaseGPUOnTerminalCrash(instance)
	}
}

// releaseGPUOnTerminalCrash frees a crashed instance's GPU pool slot once it
// is known no restart will follow (restarts exhausted, unknown instance
// type, insufficient resources to restart, or a failed relaunch attempt).
// HandleCrash's own deallocateResources call only returns vCPU/memory to the
// ResourceManager; it never touches the GPU pool, since a successful restart
// reuses the instance's still-set GPUAttachments without reclaiming them.
// Called only from paths that leave the instance permanently in StateError,
// so releasing here cannot race a restart that still needs the slot.
func (m *Manager) releaseGPUOnTerminalCrash(instance *VM) {
	if m.deps.InstanceCleaner == nil {
		return
	}
	if err := m.deps.InstanceCleaner.ReleaseGPU(instance); err != nil {
		slog.Warn("Failed to release GPU for crashed instance settling in error state",
			"instance", instance.ID, "err", err)
	}
	if len(instance.GPUAttachments) > 0 {
		m.UpdateState(instance.ID, func(v *VM) { v.GPUAttachments = nil })
	}
}
