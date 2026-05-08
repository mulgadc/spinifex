package vm

import (
	"errors"
	"maps"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
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

func TestAreVolumeSocketsValid_NoRequests(t *testing.T) {
	assert.True(t, AreVolumeSocketsValid(&VM{ID: "i-empty"}),
		"a VM with zero NBD requests has nothing to probe and must report valid")
}

func TestAreVolumeSocketsValid_EmptyURI(t *testing.T) {
	v := withNBDRequests("i-empty-uri", StateRunning, "")
	assert.True(t, AreVolumeSocketsValid(v),
		"empty NBDURI is skipped (volume not yet mounted), not probed")
}

func TestAreVolumeSocketsValid_TCPURI(t *testing.T) {
	v := withNBDRequests("i-tcp", StateRunning, "nbd://10.0.0.1:10809")
	assert.True(t, AreVolumeSocketsValid(v),
		"TCP NBD URI is treated as valid — recovery path can't probe a remote viperblockd")
}

func TestAreVolumeSocketsValid_UnparseableURI(t *testing.T) {
	v := withNBDRequests("i-junk", StateRunning, "not-an-nbd-uri")
	assert.True(t, AreVolumeSocketsValid(v),
		"unparseable URI must not block reconnect — fall through to the launch path's stricter check")
}

func TestAreVolumeSocketsValid_UnixSocketMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sock")
	v := withNBDRequests("i-no-sock", StateRunning, "nbd:unix:"+missing)
	assert.False(t, AreVolumeSocketsValid(v),
		"a unix NBD socket that does not accept connections is the orphan-QEMU signal")
}

func TestAreVolumeSocketsValid_UnixSocketLive(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "live.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	v := withNBDRequests("i-live", StateRunning, "nbd:unix:"+sockPath)
	assert.True(t, AreVolumeSocketsValid(v),
		"a live unix listener at the configured path means viperblock survived the daemon restart")
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

func TestRestore_NoStateStore_NoCrash(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := NewManager()
	m.SetDeps(Deps{
		NodeID:                     "test-node",
		ConsumeCleanShutdownMarker: func() bool { return true },
	})

	// loadRunningState surfaces "StateStore not wired"; Restore must
	// log and return without panic.
	require.NotPanics(t, m.Restore)
}
