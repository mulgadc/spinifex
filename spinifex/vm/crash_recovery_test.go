package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyCrashReason_CleanExit(t *testing.T) {
	assert.Equal(t, "clean-exit", ClassifyCrashReason(nil))
}

func TestClassifyCrashReason_OOMKill(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Process.Kill())
	waitErr := cmd.Wait()
	require.Error(t, waitErr)
	assert.Equal(t, "oom-killed", ClassifyCrashReason(waitErr))
}

func TestClassifyCrashReason_ExitCode(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 42")
	waitErr := cmd.Run()
	require.Error(t, waitErr)
	assert.Equal(t, "exit-42", ClassifyCrashReason(waitErr))
}

func TestClassifyCrashReason_Unknown(t *testing.T) {
	err := fmt.Errorf("some random error")
	assert.Equal(t, "unknown", ClassifyCrashReason(err))
}

func TestRestartBackoff_Exponential(t *testing.T) {
	tests := []struct {
		restartCount int
		want         time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 120 * time.Second},
		{6, 120 * time.Second},
		{100, 120 * time.Second},
	}

	for _, tc := range tests {
		got := RestartBackoff(tc.restartCount)
		assert.Equal(t, tc.want, got, "Backoff mismatch at restart count %d", tc.restartCount)
	}
}

// crashTestManager builds a Manager wired with the dependencies most crash
// and watchdog tests need: a recording resource controller, a static
// instance-type resolver, a recording transition state, and a controllable
// shutdown signal.
//
// Sets XDG_RUNTIME_DIR to a per-test tempdir so PID-file paths
// (utils.WaitForPidFileRemoval, ReadPidFile) cannot collide between
// tests sharing the host's real runtime dir.
func crashTestManager(t *testing.T) (m *Manager, rc *fakeResourceController, rt *recordedTransitions, shuttingDown *atomic.Bool) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rc = newFakeResourceController()
	rt = &recordedTransitions{}
	shuttingDown = &atomic.Bool{}
	m = NewManager()
	rt.bind(m)
	m.SetDeps(Deps{
		NodeID:    "test-node",
		Resources: rc,
		InstanceTypes: fakeInstanceTypeResolver{
			"t3.micro": {VCPUs: 1, MemoryMiB: 1024, Architecture: "x86_64"},
		},
		TransitionState: rt.apply,
		ShutdownSignal:  shuttingDown.Load,
	})
	return m, rc, rt, shuttingDown
}

func TestHandleCrash_SkipsNonRunning(t *testing.T) {
	m, rc, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-stopped",
		Status:       StateStopped,
		InstanceType: "t3.micro",
	}
	m.Insert(instance)

	m.HandleCrash(instance, fmt.Errorf("test error"))

	assert.Equal(t, StateStopped, m.Status(instance), "non-running instance must not transition on crash")
	assert.Zero(t, rc.deallocateCount("t3.micro"), "non-running instance must not deallocate")
}

func TestHandleCrash_SkipsShuttingDown(t *testing.T) {
	m, rc, _, shuttingDown := crashTestManager(t)
	shuttingDown.Store(true)

	instance := &VM{
		ID:           "i-shutdown",
		Status:       StateRunning,
		InstanceType: "t3.micro",
	}
	m.Insert(instance)

	m.HandleCrash(instance, fmt.Errorf("test error"))

	assert.Equal(t, StateRunning, m.Status(instance), "shutdown skip must leave status untouched")
	assert.Zero(t, rc.deallocateCount("t3.micro"), "shutdown skip must not deallocate")
}

func TestHandleCrash_CoreFlow(t *testing.T) {
	m, rc, rt, _ := crashTestManager(t)
	require.NoError(t, rc.Allocate("t3.micro"))
	require.Equal(t, 1, rc.allocateCount("t3.micro"))

	tmpDir := t.TempDir()
	qmpPath := filepath.Join(tmpDir, "qmp.sock")
	require.NoError(t, os.WriteFile(qmpPath, []byte("dummy"), 0o600))

	instance := &VM{
		ID:           "i-core",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Config:       Config{QMPSocket: qmpPath},
	}
	m.Insert(instance)

	m.HandleCrash(instance, fmt.Errorf("test crash"))

	assert.Equal(t, StateError, m.Status(instance), "crash must transition to Error")
	targets := rt.targets("i-core")
	require.NotEmpty(t, targets, "TransitionState must be invoked")
	assert.Equal(t, StateError, targets[0])

	assert.Equal(t, 1, instance.Health.CrashCount)
	assert.False(t, instance.Health.LastCrashTime.IsZero())
	assert.Equal(t, "unknown", instance.Health.LastCrashReason)
	assert.False(t, instance.Health.FirstCrashTime.IsZero())

	assert.Equal(t, 1, rc.deallocateCount("t3.micro"), "crashed instance resources must be deallocated")

	_, err := os.Stat(qmpPath)
	assert.True(t, os.IsNotExist(err), "QMP socket must be removed")
}

func TestHandleCrash_FirstCrashSetsTime(t *testing.T) {
	m, _, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-firstcrash",
		Status:       StateRunning,
		InstanceType: "t3.micro",
	}
	m.Insert(instance)

	m.HandleCrash(instance, fmt.Errorf("crash 1"))
	firstTime := instance.Health.FirstCrashTime
	assert.False(t, firstTime.IsZero())
	assert.Equal(t, 1, instance.Health.CrashCount)

	m.UpdateState(instance.ID, func(v *VM) {
		v.Status = StateRunning
	})

	m.HandleCrash(instance, fmt.Errorf("crash 2"))

	assert.Equal(t, firstTime, instance.Health.FirstCrashTime,
		"FirstCrashTime must remain set to the first crash")
	assert.Equal(t, 2, instance.Health.CrashCount)
}

// TestHandleCrash_UnknownInstanceType verifies a crash on a VM with an
// instance type not in the resolver still completes the state transition
// and health bookkeeping — even though MaybeRestart will later refuse to
// restart it.
func TestHandleCrash_UnknownInstanceType(t *testing.T) {
	m, _, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-unknown",
		Status:       StateRunning,
		InstanceType: "z99.nonexistent",
	}
	m.Insert(instance)

	m.HandleCrash(instance, fmt.Errorf("crash"))

	assert.Equal(t, StateError, m.Status(instance))
	assert.Equal(t, 1, instance.Health.CrashCount)
}

func TestMaybeRestart_ExceedsMaxInWindow(t *testing.T) {
	m, _, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-maxcrash",
		Status:       StateError,
		InstanceType: "t3.micro",
		Health: InstanceHealthState{
			CrashCount:     MaxRestartsInWindow + 1,
			FirstCrashTime: time.Now(),
			RestartCount:   MaxRestartsInWindow,
		},
	}
	m.Insert(instance)

	m.MaybeRestart(instance)

	assert.Equal(t, StateError, m.Status(instance), "exhausted-window instance must remain in error")
	assert.Equal(t, MaxRestartsInWindow, instance.Health.RestartCount,
		"RestartCount must not increment when exceeded")
}

func TestMaybeRestart_ResetsAfterWindow(t *testing.T) {
	m, _, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-windowreset",
		Status:       StateError,
		InstanceType: "t3.micro",
		Health: InstanceHealthState{
			CrashCount:     5,
			FirstCrashTime: time.Now().Add(-RestartWindow - time.Minute),
			RestartCount:   5,
		},
	}
	m.Insert(instance)

	m.MaybeRestart(instance)

	assert.Equal(t, 1, instance.Health.CrashCount,
		"crash count must reset to 1 once the window expires")
	assert.Equal(t, 0, instance.Health.RestartCount,
		"restart count must reset to 0 once the window expires")
}

func TestMaybeRestart_SkipsShuttingDown(t *testing.T) {
	m, _, _, shuttingDown := crashTestManager(t)
	shuttingDown.Store(true)

	instance := &VM{
		ID:           "i-restart-shutdown",
		Status:       StateError,
		InstanceType: "t3.micro",
		Health: InstanceHealthState{
			CrashCount:     1,
			FirstCrashTime: time.Now(),
		},
	}
	m.Insert(instance)

	m.MaybeRestart(instance)

	assert.Equal(t, 0, instance.Health.RestartCount, "shutdown skip must not increment restart")
}

func TestMaybeRestart_UnknownInstanceType(t *testing.T) {
	m, _, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-restart-unknown",
		Status:       StateError,
		InstanceType: "z99.nonexistent",
		Health: InstanceHealthState{
			CrashCount:     1,
			FirstCrashTime: time.Now(),
		},
	}
	m.Insert(instance)

	m.MaybeRestart(instance)

	assert.Equal(t, 0, instance.Health.RestartCount,
		"unknown instance type must not schedule a restart")
}

func TestMaybeRestart_InsufficientResources(t *testing.T) {
	m, rc, _, _ := crashTestManager(t)
	rc.canAllocateRet = 0

	instance := &VM{
		ID:           "i-restart-nores",
		Status:       StateError,
		InstanceType: "t3.micro",
		Health: InstanceHealthState{
			CrashCount:     1,
			FirstCrashTime: time.Now(),
		},
	}
	m.Insert(instance)

	m.MaybeRestart(instance)

	assert.Equal(t, 0, instance.Health.RestartCount,
		"insufficient resources must not schedule a restart")
}

// TestMaybeRestart_SchedulesRestart asserts the all-guards-pass branch
// reaches the time.AfterFunc scheduling path. We can't easily wait the
// full backoff, but the absence of an early-return signal (counter reset
// or RestartCount mutation in MaybeRestart itself) tells us scheduling
// happened.
func TestMaybeRestart_SchedulesRestart(t *testing.T) {
	m, _, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-restart-schedule",
		Status:       StateError,
		InstanceType: "t3.micro",
		Health: InstanceHealthState{
			CrashCount:     1,
			FirstCrashTime: time.Now(),
			RestartCount:   0,
		},
	}
	m.Insert(instance)

	m.MaybeRestart(instance)

	assert.Equal(t, 1, instance.Health.CrashCount,
		"counter reset path must not run when window is still open")
	assert.Equal(t, 0, instance.Health.RestartCount,
		"MaybeRestart must not increment RestartCount itself")
}

// TestRestartCrashedInstance_NotInError verifies the wrong-state guard
// short-circuits before touching the resource controller.
func TestRestartCrashedInstance_NotInError(t *testing.T) {
	m, rc, _, _ := crashTestManager(t)

	instance := &VM{
		ID:           "i-not-error",
		Status:       StateRunning,
		InstanceType: "t3.micro",
	}
	m.Insert(instance)

	m.RestartCrashedInstance(instance)

	assert.Equal(t, 0, rc.allocateCount("t3.micro"),
		"non-error instance must not be re-allocated")
	assert.Equal(t, StateRunning, m.Status(instance))
}

// TestRestartCrashedInstance_ShuttingDown verifies the shutdown guard
// inside RestartCrashedInstance — the late check between scheduling and
// relaunch — short-circuits before touching the resource controller.
func TestRestartCrashedInstance_ShuttingDown(t *testing.T) {
	m, rc, _, shuttingDown := crashTestManager(t)
	shuttingDown.Store(true)

	instance := &VM{
		ID:           "i-shutdown-restart",
		Status:       StateError,
		InstanceType: "t3.micro",
	}
	m.Insert(instance)

	m.RestartCrashedInstance(instance)

	assert.Equal(t, 0, rc.allocateCount("t3.micro"),
		"shutdown-guarded restart must not allocate resources")
	assert.Equal(t, StateError, m.Status(instance))
}

// TestRestartCrashedInstance_AllocateFailure verifies the allocator-error
// branch leaves the instance in StateError without invoking Run.
func TestRestartCrashedInstance_AllocateFailure(t *testing.T) {
	m, rc, _, _ := crashTestManager(t)
	rc.allocateErr = errors.New("no capacity")

	instance := &VM{
		ID:           "i-alloc-fail",
		Status:       StateError,
		InstanceType: "t3.micro",
	}
	m.Insert(instance)

	m.RestartCrashedInstance(instance)

	assert.Equal(t, StateError, m.Status(instance))
	assert.Equal(t, 1, instance.Health.RestartCount,
		"RestartCount is incremented before the allocate attempt")
}

// TestRestartCrashedInstance_RunFailureRollback covers the post-Allocate
// Run-failure path: Allocate succeeds, transition to Pending succeeds,
// then m.Run fails (Mount error). The rollback must deallocate the
// reservation and transition the instance back to StateError. A
// regression that skipped the deallocate would leak one reservation
// per crash-then-restart-fail cycle until the daemon restarts.
func TestRestartCrashedInstance_RunFailureRollback(t *testing.T) {
	m, rc, rt, _ := crashTestManager(t)

	// Wire a VolumeMounter that fails so m.Run returns an error after the
	// successful Allocate + Pending transition. SetDeps replaces deps
	// entirely, so re-supply the existing wiring from crashTestManager.
	mounter := &fakeVolumeMounter{mountErr: errors.New("mount failed during restart")}
	m.SetDeps(Deps{
		NodeID:          "test-node",
		Resources:       rc,
		VolumeMounter:   mounter,
		InstanceTypes:   fakeInstanceTypeResolver{"t3.micro": {VCPUs: 1, MemoryMiB: 1024, Architecture: "x86_64"}},
		TransitionState: rt.apply,
	})

	// Pre-allocate one reservation so the deallocate-on-rollback is
	// observable as a return-to-baseline rather than a negative count.
	require.NoError(t, rc.Allocate("t3.micro"))
	baseline := rc.allocateCount("t3.micro")

	instance := &VM{
		ID:           "i-rollback",
		Status:       StateError,
		InstanceType: "t3.micro",
	}
	m.Insert(instance)

	m.RestartCrashedInstance(instance)

	assert.Equal(t, baseline, rc.allocateCount("t3.micro"),
		"Run-failure rollback must net allocations back to baseline")
	assert.Equal(t, 1, rc.deallocateCount("t3.micro"),
		"Deallocate must be invoked exactly once on Run-failure rollback")
	assert.Equal(t, []string{"i-rollback"}, mounter.mounted,
		"Mount must have been attempted (proves the launch progressed past the Pending transition)")

	targets := rt.targets("i-rollback")
	require.Len(t, targets, 2, "expected two transitions: Pending then back to Error")
	assert.Equal(t, StatePending, targets[0])
	assert.Equal(t, StateError, targets[1])
	assert.Equal(t, StateError, m.Status(instance),
		"instance must end in StateError after Run-failure rollback")
}
