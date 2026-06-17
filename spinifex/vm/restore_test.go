package vm

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withNBDRequests returns a VM whose EBSRequests slice carries the given
// URIs in the order supplied. Each request is named "vol-<i>" so test
// failures can identify which entry tripped a probe.
func withNBDRequests(id string, status InstanceState, uris ...string) *VM {
	v := &VM{ID: id, Status: status}
	v.EBSRequests.Requests = make([]types.EBSRequest, len(uris))
	for i, uri := range uris {
		v.EBSRequests.Requests[i].Name = "vol-" + string(rune('a'+i))
		v.EBSRequests.Requests[i].NBDURI = uri
	}
	return v
}

func TestAreVolumeSocketsValid(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live.sock")
	ln, err := net.Listen("unix", live)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	missing := filepath.Join(t.TempDir(), "missing.sock")

	tests := []struct {
		name string
		vm   *VM
		want bool
		why  string
	}{
		{"NoRequests", &VM{ID: "i-empty"}, true,
			"a VM with zero NBD requests has nothing to probe and must report valid"},
		{"EmptyURI", withNBDRequests("i-empty-uri", StateRunning, ""), true,
			"empty NBDURI is skipped (volume not yet mounted), not probed"},
		{"TCPURI", withNBDRequests("i-tcp", StateRunning, "nbd://10.0.0.1:10809"), true,
			"TCP NBD URI is treated as valid — recovery path can't probe a remote viperblockd"},
		{"UnparseableURI", withNBDRequests("i-junk", StateRunning, "not-an-nbd-uri"), true,
			"unparseable URI must not block reconnect — fall through to the launch path's stricter check"},
		{"UnixSocketMissing", withNBDRequests("i-no-sock", StateRunning, "nbd:unix:"+missing), false,
			"a unix NBD socket that does not accept connections is the orphan-QEMU signal"},
		{"UnixSocketLive", withNBDRequests("i-live", StateRunning, "nbd:unix:"+live), true,
			"a live unix listener at the configured path means viperblock survived the daemon restart"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AreVolumeSocketsValid(tt.vm), tt.why)
		})
	}
}

func TestAreVolumeSocketsValid_OneOfManyMissing(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live.sock")
	dead := filepath.Join(t.TempDir(), "dead.sock")
	ln, err := net.Listen("unix", live)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	v := withNBDRequests("i-mixed", StateRunning, "nbd:unix:"+live, "nbd:unix:"+dead)
	assert.False(t, AreVolumeSocketsValid(v),
		"a single dead socket is enough to invalidate the whole instance")
}

// classifyTestManager wires a manager with an in-memory StateStore plus
// the InstanceTypes resolver and Resources controller that
// classifyRestoredInstances consults. Returns the manager and the
// fakes for assertion.
func classifyTestManager(t *testing.T) (*Manager, *fakeStateStore, *fakeResourceController) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store := newFakeStateStore()
	rc := newFakeResourceController()
	m := NewManager()
	m.SetDeps(Deps{
		NodeID:        "test-node",
		StateStore:    store,
		Resources:     rc,
		InstanceTypes: fakeInstanceTypeResolver{"t3.micro": {VCPUs: 1, MemoryMiB: 1024, Architecture: "x86_64"}},
	})
	return m, store, rc
}

func TestClassifyRestoredInstances_TerminatedMigrates(t *testing.T) {
	m, store, _ := classifyTestManager(t)
	v := &VM{ID: "i-term", Status: StateTerminated, InstanceType: "t3.micro"}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	assert.Empty(t, toLaunch, "terminated must not be queued for relaunch")
	assert.NotNil(t, store.terminated[v.ID], "terminated must be migrated to the shared bucket")
	_, stillLocal := m.Get(v.ID)
	assert.False(t, stillLocal, "terminated must be removed from the local map")
}

func TestClassifyRestoredInstances_StoppedMigrates(t *testing.T) {
	m, store, _ := classifyTestManager(t)
	v := &VM{ID: "i-stopped", Status: StateStopped, InstanceType: "t3.micro"}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	assert.Empty(t, toLaunch, "stopped must not be queued for relaunch")
	assert.NotNil(t, store.stopped[v.ID], "stopped must be migrated to the shared bucket")
	_, stillLocal := m.Get(v.ID)
	assert.False(t, stillLocal, "stopped must be removed from the local map after migration")
}

func TestClassifyRestoredInstances_UnknownTypeMarkedUnschedulable(t *testing.T) {
	m, store, _ := classifyTestManager(t)
	v := &VM{
		ID:           "i-unknown",
		Status:       StateRunning,
		InstanceType: "z99.nonexistent",
		Instance:     &ec2.Instance{},
	}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	assert.Empty(t, toLaunch, "unschedulable type must not relaunch on this node")
	assert.Equal(t, StateStopped, v.Status)
	assert.NotNil(t, store.stopped[v.ID])
	require.NotNil(t, v.Instance.StateReason)
	assert.Equal(t, "Server.InsufficientInstanceCapacity", *v.Instance.StateReason.Code)
}

func TestClassifyRestoredInstances_StoppingFinalizesToStopped(t *testing.T) {
	m, store, _ := classifyTestManager(t)
	v := &VM{ID: "i-stopping", Status: StateStopping, InstanceType: "t3.micro"}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	assert.Empty(t, toLaunch)
	assert.Equal(t, StateStopped, v.Status, "Stopping → Stopped finalisation when QEMU is gone")
	assert.NotNil(t, store.stopped[v.ID])
}

func TestClassifyRestoredInstances_ShuttingDownFinalizesToTerminated(t *testing.T) {
	m, store, _ := classifyTestManager(t)
	v := &VM{ID: "i-shutdown", Status: StateShuttingDown, InstanceType: "t3.micro"}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	assert.Empty(t, toLaunch)
	assert.Equal(t, StateTerminated, v.Status, "ShuttingDown → Terminated finalisation when QEMU is gone")
	assert.NotNil(t, store.terminated[v.ID])
}

// TestClassifyRestoredInstances_RunningWithDeadPidQueuedForRelaunch
// covers the no-PID-file path: Status was Running at shutdown but no
// QEMU process is alive. classify must reset Status to Pending and
// queue the instance for relaunch.
//
// Live-PID coverage (reconnect success/failure) is intentionally not
// driven from a vm-package unit test — the SIGKILL path inside
// killOrphanedQEMU and the long blocking PID-file polls in
// MarkFailed's goroutine make process-spawn fakes too risky for a
// hermetic test. The reconnect-failure regression is covered
// separately by a daemon-side test against a real QEMU.
func TestClassifyRestoredInstances_RunningWithDeadPidQueuedForRelaunch(t *testing.T) {
	m, _, rc := classifyTestManager(t)
	v := &VM{
		ID:           "i-running-dead",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
	}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	require.Len(t, toLaunch, 1)
	assert.Same(t, v, toLaunch[0])
	assert.Equal(t, StatePending, v.Status, "QEMU exited → reset to Pending so the relaunch path accepts it")
	require.NotNil(t, v.Instance.LaunchTime, "LaunchTime must be reset for the pending watchdog")
	assert.Equal(t, 1, rc.allocateCount("t3.micro"),
		"resources must be re-allocated before queuing for relaunch")
}

func TestClassifyRestoredInstances_PendingQueuedForRelaunch(t *testing.T) {
	m, _, rc := classifyTestManager(t)
	v := &VM{
		ID:           "i-pending",
		Status:       StatePending,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
	}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	require.Len(t, toLaunch, 1)
	assert.Equal(t, StatePending, v.Status, "Pending must remain Pending — no transitional reset")
	require.NotNil(t, v.Instance.LaunchTime, "LaunchTime must be reset for the pending watchdog")
	assert.Equal(t, 1, rc.allocateCount("t3.micro"),
		"resources must be re-allocated before queuing for relaunch")
}

func TestClassifyRestoredInstances_AllocateFailureMarksUnschedulable(t *testing.T) {
	m, store, rc := classifyTestManager(t)
	rc.allocateErr = errors.New("no capacity")

	v := &VM{
		ID:           "i-no-cap",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
	}
	m.Replace(map[string]*VM{v.ID: v})

	toLaunch := m.classifyRestoredInstances()

	assert.Empty(t, toLaunch, "allocate failure must demote the instance, not relaunch it")
	assert.Equal(t, StateStopped, v.Status)
	assert.NotNil(t, store.stopped[v.ID])
	require.NotNil(t, v.Instance.StateReason)
	assert.Equal(t, "Server.InsufficientInstanceCapacity", *v.Instance.StateReason.Code)
}

// finalizeRevertStateStore fails every shared-bucket write and the
// running-state save so finalizeTransitionalRestore exercises the
// revert-on-write-failure path. Embeds fakeStateStore so we still
// satisfy the full interface.
type finalizeRevertStateStore struct {
	*fakeStateStore
}

func (finalizeRevertStateStore) WriteStoppedInstance(string, *VM) error {
	return errors.New("stopped write failed")
}
func (finalizeRevertStateStore) WriteTerminatedInstance(string, *VM) error {
	return errors.New("terminated write failed")
}
func (finalizeRevertStateStore) SaveRunningState(string, map[string]*VM) error {
	return errors.New("save failed")
}

// TestFinalizeTransitionalRestore_WriteFailureReverts covers the
// last-resort revert in finalizeTransitionalRestore: when both the
// shared-bucket migration and the running-state save fail, the
// instance's Status must revert to its pre-classify value so the next
// daemon restart retries the same path. Without the revert the
// instance would be left in an unstable in-memory state that no
// startup branch knows how to recover.
func TestFinalizeTransitionalRestore_WriteFailureReverts(t *testing.T) {
	m := NewManager()
	m.SetDeps(Deps{
		NodeID:     "test-node",
		StateStore: finalizeRevertStateStore{newFakeStateStore()},
	})

	v := &VM{ID: "i-revert", Status: StateStopping, InstanceType: "t3.micro"}
	m.Insert(v)

	ok := m.finalizeTransitionalRestore(v)

	assert.True(t, ok, "finalizeTransitionalRestore returns true on the revert path so the caller continues")
	assert.Equal(t, StateStopping, v.Status,
		"both the migrate and the save failed — Status must revert so the next restart retries cleanly")
}

// markerStateStore captures the LoadRunningState call for assertions
// against Restore's clean-shutdown handling. Returns the canned snapshot.
type markerStateStore struct {
	*fakeStateStore

	loadCalls atomic.Int32
	snapshot  map[string]*VM
}

func (s *markerStateStore) LoadRunningState(string) (map[string]*VM, error) {
	s.loadCalls.Add(1)
	out := make(map[string]*VM, len(s.snapshot))
	maps.Copy(out, s.snapshot)
	return out, nil
}

// TestRestore_HappyPath exercises Restore's clean-shutdown branch end-
// to-end with a snapshot that classifies cleanly without entering
// relaunch fan-out. Asserts: the marker callback is consulted, the
// state store is loaded, terminated/stopped instances migrate to
// their shared buckets, and the running-state snapshot is persisted.
func TestRestore_HappyPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	store := &markerStateStore{
		fakeStateStore: newFakeStateStore(),
		snapshot: map[string]*VM{
			"i-term":    {ID: "i-term", Status: StateTerminated, InstanceType: "t3.micro"},
			"i-stopped": {ID: "i-stopped", Status: StateStopped, InstanceType: "t3.micro"},
		},
	}

	markerConsumed := atomic.Bool{}
	m := NewManager()
	m.SetDeps(Deps{
		NodeID:     "test-node",
		StateStore: store,
		ConsumeCleanShutdownMarker: func() bool {
			markerConsumed.Store(true)
			return true
		},
		InstanceTypes: fakeInstanceTypeResolver{"t3.micro": {VCPUs: 1, MemoryMiB: 1024, Architecture: "x86_64"}},
		Resources:     newFakeResourceController(),
	})

	m.Restore()

	assert.True(t, markerConsumed.Load(), "Restore must consult ConsumeCleanShutdownMarker")
	assert.Equal(t, int32(1), store.loadCalls.Load(), "Restore must call LoadRunningState exactly once")
	assert.NotNil(t, store.terminated["i-term"])
	assert.NotNil(t, store.stopped["i-stopped"])
	saved, ok := store.saved["test-node"]
	require.True(t, ok, "Restore must persist the running snapshot at the end")
	assert.Empty(t, saved, "running snapshot must be empty after both instances migrated to shared buckets")
}

// findDeadPID returns a PID that is guaranteed not to be alive at the
// moment of return. It spawns "true", waits for it to exit, then reaps
// it — the PID slot is no longer claimed by a live process, so
// signal(0) on it returns ESRCH.
func findDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())
	return cmd.Process.Pid
}

func TestIsInstanceProcessRunning(t *testing.T) {
	t.Run("ReadPidFile fails returns false", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		instance := &VM{ID: "i-no-pidfile"}
		assert.False(t, isInstanceProcessRunning(instance),
			"missing PID file is the no-process signal")
	})

	t.Run("non-positive pid returns false", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "i-zero.pid"), []byte("0"), 0o600))

		instance := &VM{ID: "i-zero"}
		assert.False(t, isInstanceProcessRunning(instance),
			"pid <= 0 must short-circuit before FindProcess so a corrupt PID file cannot be probed")
	})

	t.Run("signal nil returns true", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		require.NoError(t, utils.WritePidFile("i-alive", os.Getpid()))

		instance := &VM{ID: "i-alive"}
		assert.True(t, isInstanceProcessRunning(instance),
			"a live PID we can signal(0) without error must report alive")
	})

	t.Run("signal ESRCH returns false", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		dead := findDeadPID(t)
		require.NoError(t, utils.WritePidFile("i-esrch", dead))

		instance := &VM{ID: "i-esrch"}
		assert.False(t, isInstanceProcessRunning(instance),
			"a reaped PID slot returns ESRCH from signal(0) and must be reported dead")
	})

	t.Run("signal EPERM returns false (regression guard)", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: cannot trigger EPERM by signalling PID 1")
		}
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		require.NoError(t, utils.WritePidFile("i-eperm", 1))

		instance := &VM{ID: "i-eperm"}
		assert.False(t, isInstanceProcessRunning(instance),
			"EPERM from signal(0) means a process owned by another user occupies that PID; "+
				"the conservative answer is 'not our process' so reconnect does not attach to a stranger's QEMU")
	})
}

func TestKillOrphanedQEMU(t *testing.T) {
	t.Run("ReadPidFile fails returns false without kill", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		instance := &VM{ID: "i-no-pidfile"}

		ok := killOrphanedQEMU(instance)

		assert.False(t, ok, "no PID file means no orphan to kill; caller must skip relaunch")
	})

	t.Run("non-positive pid returns false", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "i-zero.pid"), []byte("0"), 0o600))

		ok := killOrphanedQEMU(&VM{ID: "i-zero"})

		assert.False(t, ok, "pid <= 0 must not be signalled — a corrupt PID file cannot identify a real process")
	})

	// Dead-PID case covers the "signal error silently swallowed,
	// WaitForProcessExit still called" path: on Linux os.FindProcess
	// never fails, but signaling a reaped PID returns ESRCH which the
	// `_ =` discards; the subsequent WaitForProcessExit poll then sees
	// the process gone and returns nil. The function must clean up the
	// PID file even though the signal "failed".
	t.Run("dead pid: signal error swallowed, wait succeeds, pid file removed", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		dead := findDeadPID(t)
		require.NoError(t, utils.WritePidFile("i-dead", dead))

		ok := killOrphanedQEMU(&VM{ID: "i-dead"})

		assert.True(t, ok, "WaitForProcessExit returns nil for an already-dead pid; caller proceeds with relaunch")
		_, readErr := utils.ReadPidFile("i-dead")
		assert.Error(t, readErr, "PID file must be removed after a successful wait")
	})

	t.Run("success: real subprocess SIGKILLed, pid file removed", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		cmd := exec.Command("sleep", "60")
		require.NoError(t, cmd.Start())
		// Reap in the background so the kernel releases the PID entry
		// once SIGKILL fires; without this the process becomes a
		// zombie and signal(0) still succeeds, deadlocking
		// WaitForProcessExit until its 10s timeout.
		reaped := make(chan struct{})
		go func() {
			_, _ = cmd.Process.Wait()
			close(reaped)
		}()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			<-reaped
		})
		require.NoError(t, utils.WritePidFile("i-real", cmd.Process.Pid))

		ok := killOrphanedQEMU(&VM{ID: "i-real"})

		assert.True(t, ok, "SIGKILLing the live subprocess must report success")
		_, readErr := utils.ReadPidFile("i-real")
		assert.Error(t, readErr, "PID file must be removed on the success path")
	})
}

// recoveryMounter wraps a per-id behaviour map so relaunchAll tests can
// script launch outcomes (block, fail, panic) per instance.
type recoveryMounter struct {
	mu       sync.Mutex
	mounted  []string
	behavior map[string]func(*VM) error
}

func (f *recoveryMounter) Mount(v *VM) error {
	f.mu.Lock()
	f.mounted = append(f.mounted, v.ID)
	fn := f.behavior[v.ID]
	f.mu.Unlock()
	if fn != nil {
		return fn(v)
	}
	return nil
}

func (f *recoveryMounter) Unmount(*VM) error                 { return nil }
func (f *recoveryMounter) MountOne(*types.EBSRequest) error  { return nil }
func (f *recoveryMounter) UnmountOne(types.EBSRequest) error { return nil }

var _ VolumeMounter = (*recoveryMounter)(nil)

// captureSlogRestore captures slog output for the duration of t. Local
// duplicate of captureSlog in volumes_test.go to keep restore_test.go's
// dependencies self-contained.
func captureSlogRestore(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// relaunchTestManager wires the deps relaunchAll actually consults:
// VolumeMounter for Run's Mount call, InstanceCleaner for
// MarkRecoveryFailed's stopCleanup goroutine, and recordedTransitions so
// tests can assert the StateError transition.
func relaunchTestManager(t *testing.T) (*Manager, *recoveryMounter, *recordingInstanceCleaner, *recordedTransitions) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	mounter := &recoveryMounter{behavior: map[string]func(*VM) error{}}
	cleaner := &recordingInstanceCleaner{}
	rt := &recordedTransitions{}
	m := NewManager()
	rt.bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      newFakeStateStore(),
		VolumeMounter:   mounter,
		InstanceCleaner: cleaner,
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
	})
	return m, mounter, cleaner, rt
}

func TestRelaunchAll(t *testing.T) {
	t.Run("nil OnInstanceRecovering is a no-op", func(t *testing.T) {
		m, mounter, _, _ := relaunchTestManager(t)
		// Status set outside {Pending, Provisioning} so the
		// status-check at line 298 short-circuits before Run; this
		// keeps the test focused on the OnInstanceRecovering nil-guard
		// without dragging Run/Mount/MarkRecoveryFailed into the
		// expected behaviour set.
		instances := []*VM{{ID: "i-no-hook", Status: StateRunning, InstanceType: "t3.micro"}}
		for _, v := range instances {
			m.Insert(v)
		}

		require.NotPanics(t, func() { m.relaunchAll(instances) },
			"with no OnInstanceRecovering hook, the announcement loop must skip cleanly; "+
				"a missing nil-check here would NPE on first instance")
		mounter.mu.Lock()
		defer mounter.mu.Unlock()
		assert.Empty(t, mounter.mounted,
			"status-check at line 298 short-circuits before Run; Mount must not fire for a non-launchable instance")
	})

	t.Run("panic during Run is recovered and logged", func(t *testing.T) {
		buf := captureSlogRestore(t)
		m, mounter, _, _ := relaunchTestManager(t)
		instance := &VM{ID: "i-panic", Status: StatePending, InstanceType: "t3.micro"}
		m.Insert(instance)
		mounter.behavior["i-panic"] = func(*VM) error {
			panic("synthetic mount panic")
		}

		require.NotPanics(t, func() { m.relaunchAll([]*VM{instance}) },
			"relaunchAll must absorb per-goroutine panics — one bad instance must not bring down recovery")
		assert.Contains(t, buf.String(), "Panic during instance recovery",
			"recover() must record the panic at error level so operators are not blind to silent recovery failures")
		assert.Contains(t, buf.String(), "i-panic",
			"the log line must name the instance whose Run panicked")
	})

	t.Run("Run error triggers MarkRecoveryFailed", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		mounter := &recoveryMounter{behavior: map[string]func(*VM) error{}}
		cleaner := &recordingInstanceCleaner{}
		rt := &recordedTransitions{}

		saved := make(chan struct{}, 1)
		m := NewManager()
		rt.bind(m)
		var instanceDown atomic.Int64
		m.SetDeps(Deps{
			NodeID:          "test-node",
			StateStore:      &signalingStore{onSave: func() { saved <- struct{}{} }},
			VolumeMounter:   mounter,
			InstanceCleaner: cleaner,
			TransitionState: rt.apply,
			ShutdownSignal:  func() bool { return false },
			Hooks: ManagerHooks{
				OnInstanceDown: func(string) { instanceDown.Add(1) },
			},
		})

		instance := &VM{
			ID:           "i-run-fail",
			Status:       StatePending,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		}
		m.Insert(instance)

		errored := rt.waitFor(instance.ID, StateError)
		mounter.behavior["i-run-fail"] = func(*VM) error {
			return errors.New("mount failed during recovery")
		}

		m.relaunchAll([]*VM{instance})

		select {
		case <-errored:
		case <-time.After(markFailedDeadline):
			t.Fatalf("MarkRecoveryFailed did not record StateError transition within %s", markFailedDeadline)
		}

		assert.Equal(t, StateError, m.Status(instance),
			"a Run error during relaunch must drive the instance into StateError via MarkRecoveryFailed")
		require.NotNil(t, instance.Instance.StateReason)
		assert.Equal(t, "Server.RecoveryFailed", *instance.Instance.StateReason.Code)
		assert.Equal(t, "recovery_launch_failed", *instance.Instance.StateReason.Message,
			"the reason string identifies the failure class so support can grep audit logs")

		select {
		case <-saved:
		case <-time.After(markFailedDeadline):
			t.Fatalf("MarkRecoveryFailed cleanup goroutine did not finish within %s", markFailedDeadline)
		}

		cleaner.mu.Lock()
		assert.Len(t, cleaner.releaseGPU, 1,
			"stopCleanup must release host-side GPU claim after recovery failure")
		cleaner.mu.Unlock()
		assert.Zero(t, instanceDown.Load(),
			"recovery failure preserves per-id subscriptions: OnInstanceDown must not fire")
	})

	t.Run("BeforeInstanceRelaunch fires once before Run on success", func(t *testing.T) {
		m, mounter, _, _ := relaunchTestManager(t)
		var hookOrder []string
		var mu sync.Mutex
		m.deps.Hooks.BeforeInstanceRelaunch = func(v *VM) error {
			mu.Lock()
			hookOrder = append(hookOrder, "hook:"+v.ID)
			mu.Unlock()
			return nil
		}
		mounter.behavior["i-hook-ok"] = func(v *VM) error {
			mu.Lock()
			hookOrder = append(hookOrder, "mount:"+v.ID)
			mu.Unlock()
			return nil
		}

		instance := &VM{
			ID:           "i-hook-ok",
			Status:       StatePending,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		}
		m.Insert(instance)

		m.relaunchAll([]*VM{instance})

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{"hook:i-hook-ok", "mount:i-hook-ok"}, hookOrder,
			"hook must fire before Run's Mount")
	})

	t.Run("BeforeInstanceRelaunch error marks recovery_failed and skips Run", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		mounter := &recoveryMounter{behavior: map[string]func(*VM) error{}}
		cleaner := &recordingInstanceCleaner{}
		rt := &recordedTransitions{}
		m := NewManager()
		rt.bind(m)
		m.SetDeps(Deps{
			NodeID:          "test-node",
			StateStore:      newFakeStateStore(),
			VolumeMounter:   mounter,
			InstanceCleaner: cleaner,
			TransitionState: rt.apply,
			ShutdownSignal:  func() bool { return false },
			Hooks: ManagerHooks{
				BeforeInstanceRelaunch: func(*VM) error {
					return errors.New("blob regeneration failed")
				},
			},
		})

		instance := &VM{
			ID:           "i-hook-fail",
			Status:       StatePending,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		}
		m.Insert(instance)
		errored := rt.waitFor(instance.ID, StateError)

		m.relaunchAll([]*VM{instance})

		select {
		case <-errored:
		case <-time.After(markFailedDeadline):
			t.Fatalf("MarkRecoveryFailed did not record StateError transition within %s", markFailedDeadline)
		}

		assert.Equal(t, StateError, m.Status(instance))
		require.NotNil(t, instance.Instance.StateReason)
		assert.Equal(t, "pre_relaunch_hook_failed", *instance.Instance.StateReason.Message)

		mounter.mu.Lock()
		defer mounter.mu.Unlock()
		assert.Empty(t, mounter.mounted, "Run must not fire when hook errors")
	})

	t.Run("status check skips instances flipped before launch", func(t *testing.T) {
		m, mounter, _, _ := relaunchTestManager(t)
		const total = 5
		instances := make([]*VM, total)
		for i := range instances {
			instances[i] = &VM{
				ID:           fmt.Sprintf("i-race-%d", i),
				Status:       StatePending,
				InstanceType: "t3.micro",
				Instance:     &ec2.Instance{},
			}
			m.Insert(instances[i])
		}

		// The first maxConcurrentRecovery instances block in Mount until
		// release is closed. While they block, relaunchAll's main loop
		// parks on `sem <- struct{}{}` waiting for a slot, so it has
		// not yet entered instance[maxConcurrentRecovery]'s goroutine.
		// We flip that instance to StateShuttingDown, then release the
		// gate. When the loop unblocks, the flipped instance's
		// m.Status(inst) read returns StateShuttingDown and the launch
		// is skipped — exactly the race the status check is for.
		release := make(chan struct{})
		gateReady := make(chan struct{}, maxConcurrentRecovery)
		for i := range maxConcurrentRecovery {
			id := instances[i].ID
			mounter.behavior[id] = func(*VM) error {
				gateReady <- struct{}{}
				<-release
				return nil
			}
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			m.relaunchAll(instances)
		}()

		for i := range maxConcurrentRecovery {
			select {
			case <-gateReady:
			case <-time.After(5 * time.Second):
				t.Fatalf("Mount call %d did not start within 5s", i)
			}
		}

		skipped := instances[maxConcurrentRecovery]
		m.UpdateState(skipped.ID, func(v *VM) { v.Status = StateShuttingDown })

		close(release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("relaunchAll did not return within 5s after release")
		}

		mounter.mu.Lock()
		defer mounter.mu.Unlock()
		mountedSet := map[string]bool{}
		for _, id := range mounter.mounted {
			mountedSet[id] = true
		}
		assert.False(t, mountedSet[skipped.ID],
			"instance flipped out of {Pending,Provisioning} between semaphore acquire and Run "+
				"must be skipped by the status guard; otherwise a concurrent terminate races with relaunch")
		assert.Equal(t, total-1, len(mounter.mounted),
			"every eligible instance must reach Mount: only the flipped one is skipped")
	})
}

// closeTrackingConn wraps net.Conn so a test can assert the connection
// was closed (e.g. by reconnectInstance's OnInstanceUp-failure branch)
// without leaking the pipe across the test boundary.
type closeTrackingConn struct {
	net.Conn

	closed atomic.Bool
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// fakeAttachQMP installs a QMPClient with closeTrackingConn on the
// supplied VM and returns the tracker so the caller can assert close
// behaviour. It does NOT spawn a heartbeat — that is the whole point of
// the seam: bypass the production heartbeat goroutine to keep the test
// hermetic.
func fakeAttachQMP(t *testing.T, instance *VM) *closeTrackingConn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	tracked := &closeTrackingConn{Conn: clientConn}
	instance.QMPClient = &qmp.QMPClient{Conn: tracked}
	return tracked
}

// errStateStore fails SaveRunningState so reconnectInstance's
// writeRunningState-error branch can be exercised without disturbing the
// rest of the StateStore interface.
type errStateStore struct {
	*fakeStateStore
}

func (errStateStore) SaveRunningState(string, map[string]*VM) error {
	return errors.New("save failed")
}

func TestReconnectInstance(t *testing.T) {
	t.Run("AttachQMP fails: error returned, hook not invoked", func(t *testing.T) {
		var hookCalls atomic.Int64
		m := NewManager()
		m.SetDeps(Deps{
			NodeID:     "test-node",
			StateStore: newFakeStateStore(),
			Hooks: ManagerHooks{
				OnInstanceUp: func(*VM) error { hookCalls.Add(1); return nil },
			},
		})

		orig := attachQMPForReconnect
		t.Cleanup(func() { attachQMPForReconnect = orig })
		attachFailure := errors.New("dial refused")
		attachQMPForReconnect = func(*Manager, *VM) error { return attachFailure }

		instance := &VM{ID: "i-attach-fail", Status: StatePending}

		err := m.reconnectInstance(instance)

		require.Error(t, err)
		require.ErrorIs(t, err, attachFailure,
			"the dial error must be wrapped so callers can match on it")
		assert.Zero(t, hookCalls.Load(),
			"OnInstanceUp must not fire when QMP is not connected — there is no live channel for NATS subs to control")
		assert.NotEqual(t, StateRunning, instance.Status,
			"a failed AttachQMP must not advertise the instance as running")
	})

	t.Run("AttachQMP succeeds, nil OnInstanceUp: state persisted, status Running", func(t *testing.T) {
		store := newFakeStateStore()
		m := NewManager()
		m.SetDeps(Deps{
			NodeID:     "test-node",
			StateStore: store,
		})

		orig := attachQMPForReconnect
		t.Cleanup(func() { attachQMPForReconnect = orig })
		var tracker *closeTrackingConn
		attachQMPForReconnect = func(_ *Manager, v *VM) error {
			tracker = fakeAttachQMP(t, v)
			return nil
		}

		instance := &VM{ID: "i-no-hook", Status: StatePending}
		m.Insert(instance)

		err := m.reconnectInstance(instance)

		require.NoError(t, err)
		assert.Equal(t, StateRunning, instance.Status,
			"a successful reconnect with no hook must flip status to Running")
		require.NotNil(t, tracker)
		assert.False(t, tracker.closed.Load(),
			"a successful reconnect must leave the QMP connection open for heartbeat reuse")
		saved, ok := store.saved["test-node"]
		require.True(t, ok, "writeRunningState must persist after reconnect")
		assert.NotNil(t, saved["i-no-hook"], "the reconnected instance must appear in the persisted snapshot")
	})

	t.Run("AttachQMP succeeds, OnInstanceUp errors: QMP closed and nil'd", func(t *testing.T) {
		m := NewManager()
		hookErr := errors.New("nats subscribe failed")
		m.SetDeps(Deps{
			NodeID:     "test-node",
			StateStore: newFakeStateStore(),
			Hooks: ManagerHooks{
				OnInstanceUp: func(*VM) error { return hookErr },
			},
		})

		orig := attachQMPForReconnect
		t.Cleanup(func() { attachQMPForReconnect = orig })
		var tracker *closeTrackingConn
		attachQMPForReconnect = func(_ *Manager, v *VM) error {
			tracker = fakeAttachQMP(t, v)
			return nil
		}

		instance := &VM{ID: "i-hook-fail", Status: StatePending}

		err := m.reconnectInstance(instance)

		require.Error(t, err)
		require.ErrorIs(t, err, hookErr,
			"the hook error must be wrapped so callers can distinguish reconnect failure from launch failure")
		require.NotNil(t, tracker)
		assert.True(t, tracker.closed.Load(),
			"a failed hook must close the QMP connection so the heartbeat exits — otherwise the goroutine leaks")
		assert.Nil(t, instance.QMPClient,
			"QMPClient must be nil after the hook failure so the next reconnect attempt does not re-use a dead client")
		assert.NotEqual(t, StateRunning, instance.Status,
			"status must NOT flip to Running when the hook fails — a half-wired instance must not be advertised live")
	})

	// OnInstanceUp succeeds, writeRunningState fails: pins the
	// documented inconsistency. The hook side effects are live, the
	// in-memory Status is Running, but the disk state is stale. The
	// caller in classifyRestoredInstances logs and continues without
	// re-reconnect; this test exists so a future fix (retry, rollback,
	// or compensating writeRunningState in the caller) can pivot off
	// the regression.
	t.Run("OnInstanceUp succeeds, writeRunningState fails: error returned, status Running in memory (documents the gap)", func(t *testing.T) {
		var hookCalls atomic.Int64
		m := NewManager()
		m.SetDeps(Deps{
			NodeID:     "test-node",
			StateStore: errStateStore{newFakeStateStore()},
			Hooks: ManagerHooks{
				OnInstanceUp: func(*VM) error { hookCalls.Add(1); return nil },
			},
		})

		orig := attachQMPForReconnect
		t.Cleanup(func() { attachQMPForReconnect = orig })
		attachQMPForReconnect = func(_ *Manager, v *VM) error {
			fakeAttachQMP(t, v)
			return nil
		}

		instance := &VM{ID: "i-persist-fail", Status: StatePending}
		m.Insert(instance)

		err := m.reconnectInstance(instance)

		require.Error(t, err, "persistence failure must surface to the caller")
		assert.Contains(t, err.Error(), "failed to persist reconnected instance state")
		assert.Equal(t, int64(1), hookCalls.Load(),
			"OnInstanceUp was invoked before the persist failure — the hook's side effects are already live")
		assert.Equal(t, StateRunning, instance.Status,
			"DOCUMENTED GAP: status is Running in memory but the disk state is stale; "+
				"classifyRestoredInstances logs and continues without re-reconnect")
	})
}
