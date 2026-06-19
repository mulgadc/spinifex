package vm

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/utils"
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

// TestStopAll_FiresOnInstanceDownAndMigratesPerVM verifies the DRAIN
// hook contract: every Running VM transitions through Stopping → Stopped,
// is migrated to the cluster-shared "stopped" bucket, fires
// OnInstanceDown once, and is removed from the local running map. The
// pre-fix StopAll only ran stopCleanup and left every VM stuck in
// Running, so a daemon restart promoted user-stopped VMs through the
// failed-recovery path and surfaced them as terminated — data-equivalent
// loss for the customer.
func TestStopAll_FiresOnInstanceDownAndMigratesPerVM(t *testing.T) {
	m, store, _, _, rt := shutdownTestManager(t)
	var (
		mu      sync.Mutex
		downIDs []string
	)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      store,
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: &recordingInstanceCleaner{},
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
		Hooks: ManagerHooks{
			OnInstanceDown: func(id string) {
				mu.Lock()
				downIDs = append(downIDs, id)
				mu.Unlock()
			},
		},
	})

	ids := []string{"i-1", "i-2", "i-3"}
	for _, id := range ids {
		m.Insert(&VM{
			ID:           id,
			Status:       StateRunning,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		})
	}

	require.NoError(t, m.StopAll())

	mu.Lock()
	defer mu.Unlock()
	assert.ElementsMatch(t, ids, downIDs,
		"StopAll must fire OnInstanceDown once per migrated VM")
	assert.Zero(t, m.Count(),
		"StopAll must remove migrated VMs from the local running map")
	for _, id := range ids {
		stored, err := store.LoadStoppedInstance(id)
		require.NoError(t, err)
		require.NotNil(t, stored, "VM %s must be migrated to the stopped KV bucket", id)
		assert.Equal(t, StateStopped, stored.Status,
			"VM %s must be persisted in StateStopped", id)
	}
}

// TestStopAll_SkipsNonRunningWithoutFiringHook verifies the precheck
// skip path: non-Running VMs (Pending, Stopping mid-flight, Stopped,
// Error, ShuttingDown, Terminated) surface as ErrInvalidTransition from
// stopOne and must not fire OnInstanceDown nor migrate to the shared
// bucket. Other in-flight handlers own those instances.
func TestStopAll_SkipsNonRunningWithoutFiringHook(t *testing.T) {
	m, store, _, _, rt := shutdownTestManager(t)
	var down atomic.Int64
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

	skipped := map[string]InstanceState{
		"i-pending":      StatePending,
		"i-stopping":     StateStopping,
		"i-stopped":      StateStopped,
		"i-shuttingdown": StateShuttingDown,
		"i-error":        StateError,
		"i-terminated":   StateTerminated,
	}
	for id, status := range skipped {
		m.Insert(&VM{
			ID:           id,
			Status:       status,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		})
	}

	require.NoError(t, m.StopAll())

	assert.Zero(t, down.Load(),
		"StopAll must not fire OnInstanceDown for non-Running VMs")
	assert.Equal(t, len(skipped), m.Count(),
		"StopAll must leave non-Running VMs in the local map for their owning handler")
	for id := range skipped {
		stored, err := store.LoadStoppedInstance(id)
		require.NoError(t, err)
		assert.Nil(t, stored,
			"non-Running VM %s must not migrate to the stopped KV bucket", id)
	}
}

// TestStopAll_WriteRunningStateFailure covers the post-fan-out persist
// guard at shutdown.go:129-132. Per-VM migration to the shared "stopped"
// bucket must still happen (so the fix's data-loss prevention contract
// holds), but the terminal writeRunningState failure must surface to
// the caller so DRAIN's ACK to the coordinator reflects the partial
// failure rather than silently advancing past the drain phase.
func TestStopAll_WriteRunningStateFailure(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	saveErr := errors.New("save failed")
	store := &failingSaveRunningStore{fakeStateStore: newFakeStateStore(), err: saveErr}
	m := NewManager()
	rt := (&recordedTransitions{}).bind(m)
	var down atomic.Int64
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

	ids := []string{"i-1", "i-2", "i-3"}
	for _, id := range ids {
		m.Insert(&VM{
			ID:           id,
			Status:       StateRunning,
			InstanceType: "t3.micro",
			Instance:     &ec2.Instance{},
		})
	}

	require.ErrorIs(t, m.StopAll(), saveErr,
		"writeRunningState failure must surface so DRAIN's ACK reflects the partial failure")

	assert.Equal(t, int64(len(ids)), down.Load(),
		"per-VM migration must still complete even when the terminal persist fails")
	for _, id := range ids {
		stored, err := store.LoadStoppedInstance(id)
		require.NoError(t, err)
		require.NotNil(t, stored,
			"VM %s must be persisted in the shared stopped bucket regardless of the running-state persist failure", id)
		assert.Equal(t, StateStopped, stored.Status)
	}
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
	instance := &VM{ID: "i-stop", GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:01:00.0"}}}

	m.stopCleanup(instance)

	if got := cleaner.releaseGPU; len(got) != 1 || got[0] != "i-stop" {
		t.Fatalf("ReleaseGPU on stopCleanup: got %v, want [i-stop]", got)
	}
	if len(cleaner.deleteVolumes) != 0 || len(cleaner.releasePublicIP) != 0 || len(cleaner.detachAndDeleteENI) != 0 || len(cleaner.removeFromPlacement) != 0 {
		t.Fatalf("stopCleanup leaked terminate-only calls: delete=%v pubip=%v eni=%v pg=%v",
			cleaner.deleteVolumes, cleaner.releasePublicIP, cleaner.detachAndDeleteENI, cleaner.removeFromPlacement)
	}
}

// TestStopCleanup_ReservationBoundReleasesAndClears proves a reservation-bound
// instance returns its slot to the reservation on stop and then detaches the
// binding, so a later start re-allocates from the general pool and terminate
// frees there too — never double-counting the reservation.
func TestStopCleanup_ReservationBoundReleasesAndClears(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rc := &countingResourceController{}
	m := NewManagerWithDeps(Deps{Resources: rc})
	instance := &VM{ID: "i-cr", InstanceType: "t3.micro", CapacityReservationId: "cr-123"}
	m.Insert(instance)

	m.stopCleanup(instance)

	if got := rc.releasedToCRID; len(got) != 1 || got[0] != "cr-123" {
		t.Fatalf("stop must release the slot to the reservation: got %v, want [cr-123]", got)
	}
	if rc.deallocations != 0 {
		t.Fatalf("stop must not free a reservation-bound instance to the general pool: deallocations=%d", rc.deallocations)
	}
	if instance.CapacityReservationId != "" {
		t.Fatalf("stop must clear the reservation binding so start uses general capacity: got %q", instance.CapacityReservationId)
	}
}

func TestTerminateCleanup_InvokesReleaseGPU(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cleaner := &recordingInstanceCleaner{}
	m := NewManagerWithDeps(Deps{InstanceCleaner: cleaner})
	instance := &VM{ID: "i-term", GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:01:00.0"}}}

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

// TestTerminate_AbsentIdempotent covers the idempotent guard at the top of
// Terminate (ADR-0003 §2, rule #1). An unknown id returns success without
// touching cleaners, transitions, or hooks so destroy retries converge.
func TestTerminate_AbsentIdempotent(t *testing.T) {
	m, cleaner, rt, down, _ := terminateTestManager(t, newFakeStateStore())

	err := m.Terminate("i-missing")

	require.NoError(t, err)
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

// TestTerminate_AlreadyTerminated_Idempotent covers the idempotent short-circuit
// for an already-terminated instance (ADR-0003 §2). Terminate returns nil and
// runs no cleanup or transition rather than failing the terminal-state precheck.
func TestTerminate_AlreadyTerminated_Idempotent(t *testing.T) {
	m, cleaner, _, down, _ := terminateTestManager(t, newFakeStateStore())
	v := &VM{ID: "i-already-terminated", Status: StateTerminated, Instance: &ec2.Instance{}}
	m.Insert(v)

	err := m.Terminate(v.ID)

	require.NoError(t, err)
	assert.Empty(t, cleaner.deleteVolumes, "terminateCleanup must not run on idempotent path")
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

// TestMarkRecoveryFailed_PreservesVolumesAndAWSResources locks down the
// siv-25 architectural invariant: when daemon recovery cannot bring a
// previously-running instance back online, the cleanup path must NOT
// invoke DeleteVolumes, ReleasePublicIP, DetachAndDeleteENI, or
// RemoveFromPlacementGroup. A regression here re-introduces the P0 data
// loss: a benign `systemctl restart spinifex-daemon` would destroy user
// volumes flagged DeleteOnTermination=true.
func TestMarkRecoveryFailed_PreservesVolumesAndAWSResources(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, _, _, cleaner, rt := shutdownTestManager(t)
	saved := make(chan struct{}, 1)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      &signalingStore{onSave: func() { saved <- struct{}{} }},
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: cleaner,
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
	})

	instance := &VM{
		ID:        "i-recovery-failed",
		Status:    StatePending,
		AccountID: "111122223333",
		Instance:  &ec2.Instance{},
	}
	m.Insert(instance)

	errored := rt.waitFor(instance.ID, StateError)
	m.MarkRecoveryFailed(instance, "recovery_launch_failed")

	require.NotNil(t, instance.Instance.StateReason,
		"MarkRecoveryFailed must populate StateReason synchronously")
	assert.Equal(t, "Server.RecoveryFailed", *instance.Instance.StateReason.Code)
	assert.Equal(t, "recovery_launch_failed", *instance.Instance.StateReason.Message)

	select {
	case <-errored:
	case <-time.After(markFailedDeadline):
		t.Fatalf("MarkRecoveryFailed did not transition to StateError within %s", markFailedDeadline)
	}

	assert.Equal(t, StateError, m.Status(instance),
		"instance must remain in StateError; never transition to terminated")

	// writeRunningState is the goroutine's last step; SaveRunningState
	// signals on the chan so we can assert on the cleaner without racing.
	select {
	case <-saved:
	case <-time.After(markFailedDeadline):
		t.Fatalf("cleanup goroutine did not write running state within %s", markFailedDeadline)
	}

	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	assert.Empty(t, cleaner.deleteVolumes,
		"recovery failure must not call DeleteVolumes — preserves user data")
	assert.Empty(t, cleaner.releasePublicIP,
		"recovery failure must not call ReleasePublicIP — instance retains its IP")
	assert.Empty(t, cleaner.detachAndDeleteENI,
		"recovery failure must not call DetachAndDeleteENI — instance retains its ENI")
	assert.Empty(t, cleaner.removeFromPlacement,
		"recovery failure must not call RemoveFromPlacementGroup")
	assert.NotEmpty(t, cleaner.releaseGPU,
		"recovery failure must release host-side GPU claim (no QEMU using it)")

	// Local map must retain the instance so a later ec2.TerminateInstances
	// can find and clean it up via the destructive path.
	_, ok := m.Get(instance.ID)
	assert.True(t, ok, "instance must stay in the local map for operator action")
}

// TestMarkRecoveryFailed_DoesNotFireOnInstanceDown verifies the per-id
// NATS subscription contract: OnInstanceDown must NOT fire on a
// recovery-failed instance, because the daemon must keep the
// ec2.cmd.<id> subscription live so the operator's terminate command
// can reach this node.
func TestMarkRecoveryFailed_DoesNotFireOnInstanceDown(t *testing.T) {
	defer goleak.VerifyNone(t)
	var down atomic.Int64
	m, _, _, _, rt := shutdownTestManager(t)
	m.SetDeps(Deps{
		NodeID:          "test-node",
		StateStore:      newFakeStateStore(),
		VolumeMounter:   &fakeVolumeMounter{},
		InstanceCleaner: &recordingInstanceCleaner{},
		TransitionState: rt.apply,
		ShutdownSignal:  func() bool { return false },
		Hooks: ManagerHooks{
			OnInstanceDown: func(string) { down.Add(1) },
		},
	})

	instance := &VM{
		ID:       "i-keep-sub",
		Status:   StatePending,
		Instance: &ec2.Instance{},
	}
	m.Insert(instance)

	errored := rt.waitFor(instance.ID, StateError)
	m.MarkRecoveryFailed(instance, "reconnect_failed")

	select {
	case <-errored:
	case <-time.After(markFailedDeadline):
		t.Fatalf("MarkRecoveryFailed did not transition to StateError within %s", markFailedDeadline)
	}

	// Give the async goroutine a moment to finish; even after completion
	// OnInstanceDown must remain zero.
	assert.Eventually(t, func() bool {
		return m.Status(instance) == StateError
	}, markFailedDeadline, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	assert.Zero(t, down.Load(),
		"recovery failure must not fire OnInstanceDown — per-id NATS subscription must stay live for operator action")
}

// TestMarkRecoveryFailed_NoOpOnTerminal verifies idempotency: an
// instance already in StateError / StateShuttingDown / StateTerminated
// must not be re-transitioned or have its StateReason overwritten.
func TestMarkRecoveryFailed_NoOpOnTerminal(t *testing.T) {
	for _, status := range []InstanceState{StateError, StateShuttingDown, StateTerminated} {
		t.Run(string(status), func(t *testing.T) {
			m, _, _, cleaner, rt := shutdownTestManager(t)
			instance := &VM{
				ID:       "i-noop-" + string(status),
				Status:   status,
				Instance: &ec2.Instance{},
			}
			m.Insert(instance)

			m.MarkRecoveryFailed(instance, "should_skip")

			assert.Empty(t, rt.snapshot(),
				"MarkRecoveryFailed must not transition an already-terminal instance")
			assert.Nil(t, instance.Instance.StateReason,
				"MarkRecoveryFailed must not overwrite StateReason on terminal instance")
			cleaner.mu.Lock()
			defer cleaner.mu.Unlock()
			assert.Empty(t, cleaner.releaseGPU,
				"MarkRecoveryFailed must not run cleanup on terminal instance")
		})
	}
}

// TestClassifyRestoredInstances_StateErrorSkipsRelaunch verifies the
// restore-side companion fix: an instance already in StateError (from a
// prior recovery failure) must NOT be queued for relaunch and must NOT
// have its resources re-allocated. Without this, every daemon restart
// would re-trigger the failing recovery loop and eventually re-destroy
// volumes once the loop hit the terminateCleanup path.
func TestClassifyRestoredInstances_StateErrorSkipsRelaunch(t *testing.T) {
	m := NewManager()
	rc := &countingResourceController{}
	m.SetDeps(Deps{
		NodeID:        "test-node",
		StateStore:    newFakeStateStore(),
		InstanceTypes: fakeInstanceTypeResolver{"t3.micro": {VCPUs: 2, MemoryMiB: 1024}},
		Resources:     rc,
	})

	code := "Server.RecoveryFailed"
	msg := "recovery_launch_failed"
	v := &VM{
		ID:           "i-recovery-error",
		Status:       StateError,
		InstanceType: "t3.micro",
		Instance:     &ec2.Instance{StateReason: &ec2.StateReason{Code: &code, Message: &msg}},
	}
	m.Insert(v)

	toLaunch := m.classifyRestoredInstances()

	assert.Empty(t, toLaunch, "StateError instance must not be queued for relaunch")
	assert.Zero(t, rc.allocations,
		"StateError instance must not re-allocate resources (already released by stopCleanup)")
	_, ok := m.Get(v.ID)
	assert.True(t, ok, "StateError instance must stay in the local map for operator action")
	assert.Equal(t, StateError, m.Status(v), "status must be preserved")
}

// countingResourceController counts how often Allocate fires so a test
// can prove the restore path skipped its resource-allocation branch. It
// also records general deallocations and reservation releases so stop
// tests can assert which pool a reservation-bound instance returns to.
type countingResourceController struct {
	allocations    int
	deallocations  int
	releasedToCRID []string
}

func (c *countingResourceController) Allocate(_ string) error { c.allocations++; return nil }
func (c *countingResourceController) Deallocate(_ string)     { c.deallocations++ }
func (c *countingResourceController) ReleaseToReservation(crID, _ string) {
	c.releasedToCRID = append(c.releasedToCRID, crID)
}
func (c *countingResourceController) CanAllocate(_ string, n int) int { return n }

// signalingStore is a no-op StateStore that fires onSave once per
// SaveRunningState call. Used by recovery-failure tests to wait for the
// cleanup goroutine's last step without polling a racy shared map.
type signalingStore struct {
	onSave func()
}

func (s *signalingStore) SaveRunningState(string, map[string]*VM) error {
	if s.onSave != nil {
		s.onSave()
	}
	return nil
}
func (s *signalingStore) LoadRunningState(string) (map[string]*VM, error) {
	return map[string]*VM{}, nil
}
func (s *signalingStore) WriteStoppedInstance(string, *VM) error    { return nil }
func (s *signalingStore) LoadStoppedInstance(string) (*VM, error)   { return nil, nil }
func (s *signalingStore) DeleteStoppedInstance(string) error        { return nil }
func (s *signalingStore) ListStoppedInstances() ([]*VM, error)      { return nil, nil }
func (s *signalingStore) WriteTerminatedInstance(string, *VM) error { return nil }
func (s *signalingStore) ListTerminatedInstances() ([]*VM, error)   { return nil, nil }
func (s *signalingStore) DeleteTerminatedInstance(string) error     { return nil }

var _ StateStore = (*signalingStore)(nil)

// TestShutdownAndUnmount_NilQMPClient_ProceedsToUnmount covers the QMP-nil
// branch in shutdownAndUnmount: no powerdown attempt is made, the PID-file
// wait still runs (returns immediately because no PID file was written),
// and the unmount step proceeds.
func TestShutdownAndUnmount_NilQMPClient_ProceedsToUnmount(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	mounter := &fakeVolumeMounter{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter})

	instance := &VM{ID: "i-noqmp", QMPClient: nil}

	require.NotPanics(t, func() {
		m.shutdownAndUnmount(instance)
	})

	mounter.mu.Lock()
	defer mounter.mu.Unlock()
	assert.Equal(t, []string{"i-noqmp"}, mounter.unmounted,
		"shutdownAndUnmount must call Unmount even when QMPClient is nil")
}

// TestShutdownAndUnmount_PowerdownSent_NoForceKill exercises the happy path:
// QMPClient is wired, system_powerdown is dispatched, the PID-file wait
// returns immediately (no PID file present), and the unmount step runs.
// The force-kill branch must not run because WaitForPidFileRemoval returned
// nil.
func TestShutdownAndUnmount_PowerdownSent_NoForceKill(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter})

	instance := &VM{ID: "i-powerdown", QMPClient: qmpClient}

	m.shutdownAndUnmount(instance)

	assert.Equal(t, []string{"system_powerdown"}, recorder.executes(),
		"shutdownAndUnmount must dispatch exactly one system_powerdown QMP command")

	mounter.mu.Lock()
	defer mounter.mu.Unlock()
	assert.Equal(t, []string{"i-powerdown"}, mounter.unmounted,
		"unmount must run after the wait returns")
}

// TestShutdownAndUnmount_PIDFileRemovedBeforeTimeout_NoForceKill seeds a
// real PID file, then concurrently removes it inside the 20s wait window.
// The wait must return nil (no timeout), so the force-kill branch never
// runs and the unmount step still fires.
func TestShutdownAndUnmount_PIDFileRemovedBeforeTimeout_NoForceKill(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	require.NoError(t, utils.WritePidFile("i-pid-removed", os.Getpid()))

	mounter := &fakeVolumeMounter{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter})

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = utils.RemovePidFile("i-pid-removed")
	}()

	instance := &VM{ID: "i-pid-removed", QMPClient: nil}

	start := time.Now()
	m.shutdownAndUnmount(instance)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, pidFileRemovalTimeout,
		"wait must return well before the configured timeout when the PID file disappears")

	mounter.mu.Lock()
	defer mounter.mu.Unlock()
	assert.Equal(t, []string{"i-pid-removed"}, mounter.unmounted,
		"unmount must run after PID file disappears")
}

// TestShutdownAndUnmount_NilVolumeMounter_SkipsUnmount covers the deps.
// VolumeMounter==nil guard at the unmount step: shutdownAndUnmount must
// not panic when the mounter is unwired (early-boot construction, partial
// recovery paths).
func TestShutdownAndUnmount_NilVolumeMounter_SkipsUnmount(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := NewManagerWithDeps(Deps{VolumeMounter: nil})

	instance := &VM{ID: "i-nomounter", QMPClient: nil}

	require.NotPanics(t, func() {
		m.shutdownAndUnmount(instance)
	})
}
