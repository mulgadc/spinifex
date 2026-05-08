package vm

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// markFailedDeadline is the upper bound on how long a MarkFailed cleanup
// goroutine may take to reach Terminated in a test. Generous enough that
// loaded CI runners do not flake; the chan-based signal makes the
// happy-path return effectively instantaneous.
const markFailedDeadline = 10 * time.Second

// shutdownTestManager wires the dependencies needed to exercise Stop /
// StopAll / Terminate / MarkFailed without standing up the daemon. The
// returned cleaner records every method invocation so tests can assert
// what cleanup ran (and what didn't).
//
// Sets XDG_RUNTIME_DIR to a per-test tempdir so PID-file paths
// (utils.WaitForPidFileRemoval, ReadPidFile) cannot collide between
// concurrent or sequential tests sharing the host's real runtime dir.
func shutdownTestManager(t *testing.T) (m *Manager, store *fakeStateStore, mounter *fakeVolumeMounter, cleaner *recordingInstanceCleaner, rt *recordedTransitions) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	store = newFakeStateStore()
	mounter = &fakeVolumeMounter{}
	cleaner = &recordingInstanceCleaner{}
	rt = &recordedTransitions{}
	m = NewManager()
	rt.bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   mounter,
		InstanceCleaner: cleaner,
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
	})
	return m, store, mounter, cleaner, rt
}

// TestMarkFailed_TransitionsToTerminated verifies the synchronous +
// asynchronous parts of MarkFailed: the synchronous transition to
// ShuttingDown plus the StateReason mutation, and the goroutine driving
// the instance through terminateCleanup → Terminated.
func TestMarkFailed_TransitionsToTerminated(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, _, _, _, rt := shutdownTestManager(t)

	instance := &VM{
		ID:        "i-mark-failed",
		Status:    StatePending,
		AccountID: "111122223333",
		Instance:  &ec2.Instance{},
	}
	m.Insert(instance)

	terminated := rt.waitFor(instance.ID, StateTerminated)
	m.MarkFailed(instance, "volume_preparation_failed")

	require.NotNil(t, instance.Instance.StateReason,
		"MarkFailed must populate StateReason synchronously")
	assert.Equal(t, "Server.InternalError", *instance.Instance.StateReason.Code)
	assert.Equal(t, "volume_preparation_failed", *instance.Instance.StateReason.Message)

	select {
	case <-terminated:
	case <-time.After(markFailedDeadline):
		t.Fatalf("MarkFailed cleanup goroutine did not reach Terminated within %s", markFailedDeadline)
	}

	assert.Equal(t, StateTerminated, m.Status(instance),
		"Status must be Terminated once the Terminated transition has been recorded")
	targets := rt.targets("i-mark-failed")
	require.NotEmpty(t, targets)
	assert.Equal(t, StateShuttingDown, targets[0],
		"first transition must be ShuttingDown")
	assert.Contains(t, targets, StateTerminated,
		"terminal transition must reach Terminated")
}

// TestMarkFailed_NilInstance verifies MarkFailed tolerates a VM with no
// embedded ec2.Instance (Instance == nil) without panicking.
func TestMarkFailed_NilInstance(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, _, _, _, rt := shutdownTestManager(t)

	instance := &VM{
		ID:        "i-mark-failed-nil",
		Status:    StatePending,
		AccountID: "111122223333",
		Instance:  nil,
	}
	m.Insert(instance)

	terminated := rt.waitFor(instance.ID, StateTerminated)
	require.NotPanics(t, func() {
		m.MarkFailed(instance, "test_failure")
	})

	select {
	case <-terminated:
	case <-time.After(markFailedDeadline):
		t.Fatalf("MarkFailed cleanup goroutine did not reach Terminated within %s", markFailedDeadline)
	}
	assert.Equal(t, StateTerminated, m.Status(instance))
}

// TestMarkFailed_AlreadyShuttingDown_NoOp verifies MarkFailed skips its
// work when the instance is already past pending — the existing cleanup
// goroutine owns the transition.
func TestMarkFailed_AlreadyShuttingDown_NoOp(t *testing.T) {
	m, _, _, cleaner, rt := shutdownTestManager(t)

	instance := &VM{
		ID:       "i-already-down",
		Status:   StateShuttingDown,
		Instance: &ec2.Instance{},
	}
	m.Insert(instance)

	m.MarkFailed(instance, "duplicate_call")

	assert.Empty(t, rt.snapshot(),
		"MarkFailed must not transition an already-shutting-down instance")
	assert.Nil(t, instance.Instance.StateReason,
		"MarkFailed must not overwrite StateReason on already-shutting-down instance")
	assert.Zero(t, cleaner.deleteVolumesCount(),
		"MarkFailed must not run cleanup on already-shutting-down instance")
}

func TestMarkFailed_AlreadyTerminated_NoOp(t *testing.T) {
	m, _, _, _, rt := shutdownTestManager(t)

	instance := &VM{
		ID:       "i-terminated",
		Status:   StateTerminated,
		Instance: &ec2.Instance{},
	}
	m.Insert(instance)

	m.MarkFailed(instance, "duplicate_call")

	assert.Empty(t, rt.snapshot(),
		"MarkFailed must not transition an already-terminated instance")
}

// TestStop_DoesNotCallDeleteVolumes locks down the architectural
// invariant that Stop must never delete volumes — a regression here
// would silently destroy user data on every stop.
func TestStop_DoesNotCallDeleteVolumes(t *testing.T) {
	m, _, _, cleaner, _ := shutdownTestManager(t)

	instance := &VM{
		ID:           "i-stop-nodelete",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
	}
	m.Insert(instance)

	require.NoError(t, m.Stop(instance.ID))

	assert.Zero(t, cleaner.deleteVolumesCount(),
		"Manager.Stop must never invoke InstanceCleaner.DeleteVolumes")
}

// TestStopAll_DoesNotCallDeleteVolumes mirrors the Stop invariant for the
// fan-out path used by coordinated shutdown / SIGTERM.
func TestStopAll_DoesNotCallDeleteVolumes(t *testing.T) {
	m, _, _, cleaner, _ := shutdownTestManager(t)

	for i := range 3 {
		m.Insert(&VM{
			ID:           string(rune('a' + i)),
			Status:       StateRunning,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		})
	}

	require.NoError(t, m.StopAll())

	assert.Zero(t, cleaner.deleteVolumesCount(),
		"Manager.StopAll must never invoke InstanceCleaner.DeleteVolumes")
}

func TestStopAll_EmptyMap_FastPath(t *testing.T) {
	m, _, mounter, cleaner, rt := shutdownTestManager(t)

	require.NoError(t, m.StopAll())

	assert.Empty(t, rt.snapshot(), "empty StopAll must not invoke transitions")
	assert.Empty(t, mounter.unmounted, "empty StopAll must not invoke unmount")
	assert.Empty(t, cleaner.cleanupMgmt, "empty StopAll must not invoke cleaner")
}

// TestStopAll_DoesNotFireOnInstanceDown verifies StopAll's fan-out
// shutdown path — used by coordinated DRAIN and SIGTERM — leaves the
// hook contract alone. Per the plan's hook contract, Stop fires
// OnInstanceDown but StopAll does not (it leaves instances in the
// running map for restoreInstances to pick up on next boot).
func TestStopAll_DoesNotFireOnInstanceDown(t *testing.T) {
	m, _, _, _, _ := shutdownTestManager(t)
	var down atomic.Int64
	rt := (&recordedTransitions{}).bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
		Hooks: ManagerHooks{
			OnInstanceDown: func(string) { down.Add(1) },
		},
	})

	for _, id := range []string{"i-1", "i-2", "i-3"} {
		m.Insert(&VM{
			ID:           id,
			Status:       StateRunning,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		})
	}

	require.NoError(t, m.StopAll())

	assert.Zero(t, down.Load(),
		"StopAll must not fire OnInstanceDown — instances stay in the running map")
	assert.Equal(t, 3, m.Count(),
		"StopAll must leave instances in the local map for restoreInstances")
}

// TestMigrateStoppedToSharedKV_SlotReclaim covers the DeleteIf-mismatch
// branch: another handler reclaimed the slot under the same id while the
// shared write was in flight, so the local delete must not happen and
// the function must report false.
func TestMigrateStoppedToSharedKV_SlotReclaim(t *testing.T) {
	m, store, _, _, _ := shutdownTestManager(t)

	original := &VM{ID: "i-reclaim", Status: StateStopped}
	reclaimed := &VM{ID: "i-reclaim", Status: StateRunning}
	m.Insert(original)
	// Replace the slot under the same id — DeleteIf(original) must miss.
	m.Insert(reclaimed)

	got := m.MigrateStoppedToSharedKV(original)
	require.False(t, got, "MigrateStoppedToSharedKV must return false when the slot was reclaimed")

	// The shared KV write still happened (writeFn fires before DeleteIf).
	stored, _ := store.LoadStoppedInstance("i-reclaim")
	assert.NotNil(t, stored, "shared KV write must precede the slot check")

	// The reclaimed instance must remain in the local map.
	v, ok := m.Get("i-reclaim")
	require.True(t, ok, "reclaimed slot must remain in the local map")
	assert.Same(t, reclaimed, v, "Get must return the reclaimed pointer, not the original")
}

func TestMigrateStoppedToSharedKV_KVWriteFailure(t *testing.T) {
	mounter := &fakeVolumeMounter{}
	cleaner := &recordingInstanceCleaner{}
	store := failOnWriteStoppedStore{newFakeStateStore(), errors.New("kv unreachable")}
	m := NewManagerWithDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   mounter,
		InstanceCleaner: cleaner,
		ShutdownSignal:  func() bool { return false },
	})

	v := &VM{ID: "i-kv-fail", Status: StateStopped}
	m.Insert(v)

	got := m.MigrateStoppedToSharedKV(v)
	assert.False(t, got, "MigrateStoppedToSharedKV must return false on KV write failure")

	// Local map entry must remain so restoreInstances can retry on boot.
	_, ok := m.Get("i-kv-fail")
	assert.True(t, ok, "KV write failure must leave the instance in the local map")
}

// TestMigrateStoppedToSharedKV_NoStateStore covers the no-deps fallback
// where StateStore is nil — used by Manager built via NewManager() with
// no Deps wired (e.g. early-boot daemon construction).
func TestMigrateStoppedToSharedKV_NoStateStore(t *testing.T) {
	m := NewManager()
	v := &VM{ID: "i-no-store", Status: StateStopped}
	m.Insert(v)

	got := m.MigrateStoppedToSharedKV(v)
	assert.False(t, got, "missing StateStore must report false (no migration possible)")
	_, ok := m.Get("i-no-store")
	assert.True(t, ok, "missing StateStore must leave local map untouched")
}

// failOnWriteStoppedStore wraps fakeStateStore and forces the
// WriteStoppedInstance path to error so the slot-reclaim contract can be
// verified in isolation from the happy path.
type failOnWriteStoppedStore struct {
	*fakeStateStore

	err error
}

func (f failOnWriteStoppedStore) WriteStoppedInstance(string, *VM) error {
	return f.err
}

// TestStop_FiresOnInstanceDownExactlyOnce verifies the success-path hook
// contract: Stop fires OnInstanceDown once after a successful transition
// to Stopped + shared-KV migration.
func TestStop_FiresOnInstanceDownExactlyOnce(t *testing.T) {
	m, _, _, _, _ := shutdownTestManager(t)
	var down atomic.Int64
	var downIDs []string
	var mu sync.Mutex
	rt := (&recordedTransitions{}).bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      newFakeStateStore(),
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: &recordingInstanceCleaner{},
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
		Hooks: ManagerHooks{
			OnInstanceDown: func(id string) {
				down.Add(1)
				mu.Lock()
				downIDs = append(downIDs, id)
				mu.Unlock()
			},
		},
	})

	v := &VM{
		ID:           "i-stop-hook",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
	}
	m.Insert(v)

	require.NoError(t, m.Stop(v.ID))

	assert.Equal(t, int64(1), down.Load(), "Stop must fire OnInstanceDown exactly once")
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"i-stop-hook"}, downIDs)
}

// TestStop_DoesNotFireOnInstanceDown_OnSlotReclaim verifies the slot-
// reclaim branch in Stop: when MigrateStoppedToSharedKV returns false
// because a concurrent handler took the slot, OnInstanceDown must not
// fire (firing it would tear down the new instance's NATS subs).
func TestStop_DoesNotFireOnInstanceDown_OnSlotReclaim(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var down atomic.Int64
	store := &reclaimingStateStore{fakeStateStore: newFakeStateStore()}
	m := NewManager()
	rt := (&recordedTransitions{}).bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: &recordingInstanceCleaner{},
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
		Hooks: ManagerHooks{
			OnInstanceDown: func(string) { down.Add(1) },
		},
	})
	store.m = m

	v := &VM{
		ID:           "i-reclaim-no-hook",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{},
	}
	m.Insert(v)

	require.NoError(t, m.Stop(v.ID))

	assert.Zero(t, down.Load(),
		"slot-reclaim branch must not fire OnInstanceDown")
}

// reclaimingStateStore reclaims the slot under the same id during the
// shared-KV write callback, so the subsequent DeleteIf in
// migrateInstanceToKV finds a different pointer and returns false.
type reclaimingStateStore struct {
	*fakeStateStore

	m *Manager
}

func (r *reclaimingStateStore) WriteStoppedInstance(id string, v *VM) error {
	if err := r.fakeStateStore.WriteStoppedInstance(id, v); err != nil {
		return err
	}
	// Mid-flight slot reclaim: a concurrent start handler installs a new
	// pointer under the same id between the write and DeleteIf.
	r.m.Insert(&VM{ID: id, Status: StateRunning})
	return nil
}

func TestStopCleanup_InvokesReleaseGPU(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cleaner := &recordingInstanceCleaner{}
	m := NewManagerWithDeps(Deps{InstanceCleaner: cleaner})
	instance := &VM{ID: "i-stop", GPUPCIAddress: "0000:01:00.0"}

	m.stopCleanup(instance)

	if got := cleaner.releaseGPU; len(got) != 1 || got[0] != "i-stop" {
		t.Fatalf("ReleaseGPU on stopCleanup: got %v, want [i-stop]", got)
	}
	if len(cleaner.deleteVolumes) != 0 || len(cleaner.releasePublicIP) != 0 || len(cleaner.detachAndDeleteENI) != 0 || len(cleaner.removeFromPlacement) != 0 {
		t.Fatalf("stopCleanup leaked terminate-only calls: delete=%v pubip=%v eni=%v pg=%v",
			cleaner.deleteVolumes, cleaner.releasePublicIP, cleaner.detachAndDeleteENI, cleaner.removeFromPlacement)
	}
}

func TestTerminateCleanup_InvokesReleaseGPU(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cleaner := &recordingInstanceCleaner{}
	m := NewManagerWithDeps(Deps{InstanceCleaner: cleaner})
	instance := &VM{ID: "i-term", GPUPCIAddress: "0000:01:00.0"}

	m.terminateCleanup(instance)

	if got := cleaner.releaseGPU; len(got) != 1 || got[0] != "i-term" {
		t.Fatalf("ReleaseGPU on terminateCleanup: got %v, want [i-term]", got)
	}
	if got := cleaner.deleteVolumes; len(got) != 1 || got[0] != "i-term" {
		t.Fatalf("DeleteVolumes on terminateCleanup: got %v, want [i-term]", got)
	}
}

func TestCleanup_NoGPU_StillInvokesReleaseGPU(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// The adapter no-ops for GPU-less instances; the manager must still
	// invoke the method so the adapter owns that decision rather than the
	// manager second-guessing it.
	cleaner := &recordingInstanceCleaner{}
	m := NewManagerWithDeps(Deps{InstanceCleaner: cleaner})
	instance := &VM{ID: "i-cpu"}

	m.stopCleanup(instance)
	m.terminateCleanup(instance)

	if got := len(cleaner.releaseGPU); got != 2 {
		t.Fatalf("ReleaseGPU calls across stop+terminate: got %d, want 2", got)
	}
}

// TestTransitionWithPrecheck_Raced_WrapsErrInvalidTransition covers the
// raced branch in transitionWithPrecheck: the static precheck succeeded
// (transition was valid at that moment) but Deps.TransitionState
// returned an error AND the in-memory status is no longer at target,
// meaning a concurrent goroutine flipped the instance away. The
// returned error must wrap ErrInvalidTransition so the daemon's
// handler can map it to AWS IncorrectInstanceState (and so
// daemon_handlers_instance.go's slog level keying on errors.Is
// classifies the error correctly).
func TestTransitionWithPrecheck_Raced_WrapsErrInvalidTransition(t *testing.T) {
	persistErr := errors.New("kv put failed")
	m := NewManagerWithDeps(Deps{
		TransitionState: func(_ *VM, _ InstanceState) error {
			// Return error without mutating status so the in-memory
			// state stays at the pre-transition value, matching the
			// "concurrent terminate beat us to it" scenario.
			return persistErr
		},
	})

	instance := &VM{ID: "i-raced", Status: StatePending}
	m.Insert(instance)

	err := m.transitionWithPrecheck(instance, StateRunning)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidTransition,
		"raced post-precheck failure must wrap ErrInvalidTransition so callers map to IncorrectInstanceState")
	assert.NotErrorIs(t, err, persistErr,
		"the original persistence error is intentionally hidden — caller surface is ErrInvalidTransition")
	assert.Equal(t, StatePending, m.Status(instance),
		"raced branch implies status was not flipped to target")
}

// TestTransitionWithPrecheck_PersistenceFailure_PassesThroughError is
// the contrast: TransitionState returned an error but the in-memory
// status DID reach target (the memory mutation succeeded; only the
// persistence side failed). The original error must propagate as-is
// without an ErrInvalidTransition wrap, so callers can distinguish a
// transient persistence retry from a real transition rejection.
func TestTransitionWithPrecheck_PersistenceFailure_PassesThroughError(t *testing.T) {
	persistErr := errors.New("kv put failed")
	var m *Manager
	m = NewManagerWithDeps(Deps{
		TransitionState: func(v *VM, target InstanceState) error {
			// Memory mutation succeeded; only persistence failed.
			m.Inspect(v, func(vv *VM) { vv.Status = target })
			return persistErr
		},
	})

	instance := &VM{ID: "i-persist-fail", Status: StatePending}
	m.Insert(instance)

	err := m.transitionWithPrecheck(instance, StateRunning)

	require.Error(t, err)
	require.ErrorIs(t, err, persistErr,
		"persistence-only failure must surface the original error so callers can retry/log it specifically")
	assert.NotErrorIs(t, err, ErrInvalidTransition,
		"persistence failure on a valid transition is not a transition error")
	assert.Equal(t, StateRunning, m.Status(instance),
		"persistence-failure branch implies status reached target before error returned")
}

// TestTransitionWithPrecheck_InvalidInitialTransition_WrapsErrInvalidTransition
// covers the static precheck rejecting an illegal transition before
// TransitionState is ever called. The error must wrap
// ErrInvalidTransition. Pairs with the raced branch — both surfaces
// look the same to the caller, which is the design.
func TestTransitionWithPrecheck_InvalidInitialTransition_WrapsErrInvalidTransition(t *testing.T) {
	called := false
	m := NewManagerWithDeps(Deps{
		TransitionState: func(*VM, InstanceState) error {
			called = true
			return nil
		},
	})

	instance := &VM{ID: "i-bad-trans", Status: StateTerminated}
	m.Insert(instance)

	err := m.transitionWithPrecheck(instance, StateRunning)

	require.ErrorIs(t, err, ErrInvalidTransition)
	assert.False(t, called, "Deps.TransitionState must not run when precheck rejects")
	assert.Equal(t, StateTerminated, m.Status(instance), "rejected precheck must not mutate status")
}

// failingTerminatedStateStore overrides only WriteTerminatedInstance so
// finalizeTerminated's persistence-failure path can be exercised.
type failingTerminatedStateStore struct {
	*fakeStateStore

	err error
}

func (f *failingTerminatedStateStore) WriteTerminatedInstance(string, *VM) error {
	return f.err
}

// reclaimingTerminatedStore reclaims the slot under the same id during
// the terminated-bucket write so the subsequent DeleteIf in
// finalizeTerminated finds a different pointer and returns false.
type reclaimingTerminatedStore struct {
	*fakeStateStore

	m *Manager
}

func (r *reclaimingTerminatedStore) WriteTerminatedInstance(id string, v *VM) error {
	if err := r.fakeStateStore.WriteTerminatedInstance(id, v); err != nil {
		return err
	}
	r.m.Insert(&VM{ID: id, Status: StateRunning})
	return nil
}

// failingSaveRunningStore overrides only SaveRunningState so the
// post-DeleteIf rollback path in finalizeTerminated can be exercised
// without disrupting WriteTerminatedInstance.
type failingSaveRunningStore struct {
	*fakeStateStore

	err error
}

func (f *failingSaveRunningStore) SaveRunningState(string, map[string]*VM) error {
	return f.err
}

// terminateTestManager builds a Manager wired for Terminate end-to-end
// tests with hooks counter, state store, transition recorder, and a
// recording cleaner. Returns everything callers may need to assert on.
//
// Sets XDG_RUNTIME_DIR to a per-test tempdir so PID-file paths
// (utils.WaitForPidFileRemoval, ReadPidFile invoked from
// shutdownAndUnmount) cannot collide between tests.
func terminateTestManager(t *testing.T, store StateStore) (m *Manager, cleaner *recordingInstanceCleaner, rt *recordedTransitions, downCount *atomic.Int64, downIDs *[]string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cleaner = &recordingInstanceCleaner{}
	rt = &recordedTransitions{}
	downCount = &atomic.Int64{}
	downIDs = &[]string{}
	var mu sync.Mutex
	m = NewManager()
	rt.bind(m)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: cleaner,
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
		Hooks: ManagerHooks{
			OnInstanceDown: func(id string) {
				downCount.Add(1)
				mu.Lock()
				*downIDs = append(*downIDs, id)
				mu.Unlock()
			},
		},
	})
	return m, cleaner, rt, downCount, downIDs
}

// TestTerminate_NotFound covers the validation guard at the top of
// Terminate. An unknown id must return ErrInstanceNotFound without
// touching cleaners, transitions, or hooks.
func TestTerminate_NotFound(t *testing.T) {
	m, cleaner, rt, down, _ := terminateTestManager(t, newFakeStateStore())

	err := m.Terminate("i-missing")

	require.ErrorIs(t, err, ErrInstanceNotFound)
	assert.Empty(t, cleaner.deleteVolumes)
	assert.Empty(t, rt.snapshot())
	assert.Zero(t, down.Load())
}

// TestTerminate_AlreadyShuttingDown_Idempotent covers the early-return
// at shutdown.go:113-116. A concurrent failed-launch goroutine has
// already transitioned the instance to StateShuttingDown and owns
// cleanup; Terminate must return nil and do nothing — no second
// terminateCleanup, no second transition, no second OnInstanceDown.
func TestTerminate_AlreadyShuttingDown_Idempotent(t *testing.T) {
	m, cleaner, rt, down, _ := terminateTestManager(t, newFakeStateStore())
	v := &VM{ID: "i-already-shutting", Status: StateShuttingDown, Instance: &ec2.Instance{}}
	m.Insert(v)

	err := m.Terminate(v.ID)

	require.NoError(t, err)
	assert.Empty(t, cleaner.deleteVolumes, "terminateCleanup must not run on idempotent path")
	assert.Empty(t, rt.snapshot(), "no transitions on idempotent path")
	assert.Zero(t, down.Load(), "OnInstanceDown must not fire on idempotent path")
	assert.Equal(t, StateShuttingDown, m.Status(v))
}

// TestTerminate_InvalidTransition covers transitionWithPrecheck rejecting
// an illegal initial transition. StateTerminated → StateShuttingDown is
// not in ValidTransitions, so the daemon-side handler can map this to
// AWS IncorrectInstanceState.
func TestTerminate_InvalidTransition(t *testing.T) {
	m, cleaner, _, down, _ := terminateTestManager(t, newFakeStateStore())
	v := &VM{ID: "i-already-terminated", Status: StateTerminated, Instance: &ec2.Instance{}}
	m.Insert(v)

	err := m.Terminate(v.ID)

	require.ErrorIs(t, err, ErrInvalidTransition)
	assert.Empty(t, cleaner.deleteVolumes, "terminateCleanup must not run when precheck rejects")
	assert.Zero(t, down.Load())
}

// TestTerminate_Success_FiresHooksAndCleanupOnce is the happy-path
// contract: a Running instance terminates through ShuttingDown →
// Terminated, the full terminate-only cleanup chain runs once, the
// terminated KV bucket is written, the instance leaves the local map,
// OnInstanceDown fires exactly once, and the running-state snapshot
// is persisted.
func TestTerminate_Success_FiresHooksAndCleanupOnce(t *testing.T) {
	store := newFakeStateStore()
	m, cleaner, rt, down, downIDs := terminateTestManager(t, store)

	v := &VM{ID: "i-terminate-ok", Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID))

	assert.Equal(t, []InstanceState{StateShuttingDown, StateTerminated}, rt.targets(v.ID))
	assert.Equal(t, StateTerminated, m.Status(v))
	assert.Equal(t, []string{v.ID}, cleaner.deleteVolumes, "terminate-only cleaner must run once")
	assert.Equal(t, []string{v.ID}, cleaner.releasePublicIP)
	assert.Equal(t, []string{v.ID}, cleaner.detachAndDeleteENI)
	assert.Equal(t, []string{v.ID}, cleaner.removeFromPlacement)
	assert.Equal(t, []string{v.ID}, cleaner.releaseGPU)
	assert.Equal(t, int64(1), down.Load(), "OnInstanceDown must fire exactly once on success")
	assert.Equal(t, []string{v.ID}, *downIDs)
	require.NotNil(t, store.terminated[v.ID], "WriteTerminatedInstance must persist before DeleteIf")
	_, stillInMap := m.Get(v.ID)
	assert.False(t, stillInMap, "instance must leave the local map on successful Terminate")
}

// TestTerminate_WriteTerminatedFailure_PropagatesAndKeepsLocal covers
// finalizeTerminated's persistence guard at shutdown.go:189-194. When
// the terminated-bucket write fails the error must propagate and the
// instance must stay in the local map so the next daemon restart
// can retry — DeleteIf must not run, OnInstanceDown must not fire.
func TestTerminate_WriteTerminatedFailure_PropagatesAndKeepsLocal(t *testing.T) {
	persistErr := errors.New("kv put failed")
	store := &failingTerminatedStateStore{fakeStateStore: newFakeStateStore(), err: persistErr}
	m, _, _, down, _ := terminateTestManager(t, store)

	v := &VM{ID: "i-write-fail", Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	m.Insert(v)

	err := m.Terminate(v.ID)

	require.ErrorIs(t, err, persistErr)
	assert.Zero(t, down.Load(), "OnInstanceDown must not fire when the terminated-bucket write fails")
	_, stillInMap := m.Get(v.ID)
	assert.True(t, stillInMap, "instance must remain in local map for retry on the next daemon restart")
}

// TestTerminate_DeleteIfReclaimed_NoHook covers the slot-reclaim branch
// in finalizeTerminated (shutdown.go:197-201): a concurrent handler
// inserts a fresh VM under the same id between WriteTerminatedInstance
// and DeleteIf. Terminate must return nil without firing
// OnInstanceDown — firing it would tear down the new instance's NATS
// subscriptions.
func TestTerminate_DeleteIfReclaimed_NoHook(t *testing.T) {
	store := &reclaimingTerminatedStore{fakeStateStore: newFakeStateStore()}
	m, _, _, down, _ := terminateTestManager(t, store)
	store.m = m

	v := &VM{ID: "i-reclaim-term", Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID))

	assert.Zero(t, down.Load(), "slot-reclaim must not fire OnInstanceDown")
	got, ok := m.Get(v.ID)
	require.True(t, ok, "the reclaiming inserter's pointer must still be in the map")
	assert.NotSame(t, v, got, "DeleteIf must not have removed the reclaimer's VM")
}

// TestTerminate_WriteRunningStateFailure_RollbackInsert covers the
// rollback path at shutdown.go:207-212. SaveRunningState fails after
// DeleteIf has already removed the instance; finalizeTerminated must
// re-insert it so the local map and the persisted snapshot stay
// consistent. Terminate still returns nil because the state write is
// best-effort at this layer.
func TestTerminate_WriteRunningStateFailure_RollbackInsert(t *testing.T) {
	saveErr := errors.New("save failed")
	store := &failingSaveRunningStore{fakeStateStore: newFakeStateStore(), err: saveErr}
	m, _, _, down, _ := terminateTestManager(t, store)

	v := &VM{ID: "i-saverun-fail", Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID))

	assert.Equal(t, int64(1), down.Load(), "OnInstanceDown fires before the writeRunningState rollback")
	got, ok := m.Get(v.ID)
	require.True(t, ok, "writeRunningState failure must re-insert the instance into the local map")
	assert.Same(t, v, got)
}
