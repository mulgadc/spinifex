package vm

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUEFIStores records each terminator call; has and markErr shape the mark.
type fakeUEFIStores struct {
	mu      sync.Mutex
	has     bool
	markErr error
	marked  []string
	removed []string
}

var _ UEFIStoreTerminator = (*fakeUEFIStores)(nil)

func (f *fakeUEFIStores) MarkUEFIStoreDeleting(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, id)
	return f.has && f.markErr == nil, f.markErr
}

func (f *fakeUEFIStores) RemoveLocalUEFIStore(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeUEFIStores) calls() (marked, removed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.marked...), append([]string(nil), f.removed...)
}

func withUEFIStores(m *Manager, f *fakeUEFIStores) {
	deps := m.deps
	deps.UEFIStores = f
	m.SetDeps(deps)
}

func runningUEFIInstance(id string) *VM {
	return &VM{ID: id, Status: StateRunning, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
}

// TestS8_TerminateRecordsTheUEFIStoreObligationAndReturns enforces ADR-0003:S8
// for the store: terminate deletes no object, it records the obligation and
// returns, and the terminated record carries it for the reaper to converge.
func TestS8_TerminateRecordsTheUEFIStoreObligationAndReturns(t *testing.T) {
	store := newFakeStateStore()
	m, _, _, _, _ := terminateTestManager(t, store)
	stores := &fakeUEFIStores{has: true}
	withUEFIStores(m, stores)
	v := runningUEFIInstance("i-uefi-terminate")
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID))

	marked, removed := stores.calls()
	assert.Equal(t, []string{v.ID}, marked, "terminate must record the store for deletion")
	assert.Equal(t, []string{v.ID}, removed, "the node that ran the instance removes its local copy")
	terminated := store.terminated[v.ID]
	require.NotNil(t, terminated, "terminate must write the terminated record")
	assert.Equal(t, string(TeardownPending), terminated.Teardown[TeardownUEFI],
		"ADR-0003:S8: the terminated record must carry the store obligation while the reaper converges it")
	assert.False(t, terminated.TeardownComplete(), "a pending store keeps the record from early purge")
}

func TestTerminate_InstanceWithNoUEFIStoreStampsNoEntry(t *testing.T) {
	store := newFakeStateStore()
	m, _, _, _, _ := terminateTestManager(t, store)
	withUEFIStores(m, &fakeUEFIStores{has: false})
	v := runningUEFIInstance("i-bios")
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID))

	_, stamped := store.terminated[v.ID].Teardown[TeardownUEFI]
	assert.False(t, stamped, "a BIOS instance has no store, so nothing is owed")
}

// A mark that fails is stamped failed rather than dropped, so the reaper can
// complete it from the terminated record.
func TestTerminate_FailedUEFIMarkIsStampedFailed(t *testing.T) {
	store := newFakeStateStore()
	m, _, _, _, _ := terminateTestManager(t, store)
	withUEFIStores(m, &fakeUEFIStores{has: true, markErr: errors.New("kv unavailable")})
	v := runningUEFIInstance("i-uefi-mark-fails")
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID), "a store mark never fails the terminate")

	assert.Equal(t, string(TeardownFailed), store.terminated[v.ID].Teardown[TeardownUEFI])
}

// A terminate that wedged before its cleanup still records the store when the
// backstop finalises it.
func TestReconcileVanishedQEMU_RecordsTheUEFIStoreForDeletion(t *testing.T) {
	store := newFakeStateStore()
	m, _, _, _, _ := terminateTestManager(t, store)
	stores := &fakeUEFIStores{has: true}
	withUEFIStores(m, stores)
	v := runningUEFIInstance("i-uefi-wedged")
	v.Status = StateShuttingDown
	m.Insert(v)

	require.NoError(t, m.reconcileVanishedQEMU(v))

	marked, _ := stores.calls()
	assert.Equal(t, []string{v.ID}, marked)
	assert.Equal(t, string(TeardownPending), store.terminated[v.ID].Teardown[TeardownUEFI])
}

// TestS7_OnlyTerminateRecordsTheUEFIStoreForDeletion enforces ADR-0003:S7 at
// the node: a stop, a node-wide stop and a failed recovery of an instance
// that has run never move its store toward deletion or remove its local copy.
func TestS7_OnlyTerminateRecordsTheUEFIStoreForDeletion(t *testing.T) {
	m, _, _, _, rt := shutdownTestManager(t)
	stores := &fakeUEFIStores{has: true}
	withUEFIStores(m, stores)

	stopped := runningUEFIInstance("i-uefi-stop")
	m.Insert(stopped)
	require.NoError(t, m.Stop(stopped.ID))

	drained := runningUEFIInstance("i-uefi-drain")
	m.Insert(drained)
	require.NoError(t, m.StopAll())

	failed := &VM{ID: "i-uefi-has-run", Status: StatePending, InstanceType: "t3.micro", Instance: &ec2.Instance{}}
	m.Insert(failed)
	errored := rt.waitFor(failed.ID, StateError)
	m.MarkFailed(t.Context(), failed, "relaunch_timeout")
	select {
	case <-errored:
	case <-time.After(markFailedDeadline):
		t.Fatalf("MarkFailed on an instance that has run did not reach error within %s", markFailedDeadline)
	}
	m.goroutineWg.Wait()

	marked, removed := stores.calls()
	assert.Empty(t, marked, "ADR-0003:S7: only a decided terminate may record a store for deletion")
	assert.Empty(t, removed, "ADR-0003:S7: a stop or failure keeps the node's copy of the store")
}
