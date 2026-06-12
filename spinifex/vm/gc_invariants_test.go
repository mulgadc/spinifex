package vm

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTerminated writes a terminated VM owned by the test node with the given
// teardown marks into the store, and returns the manager + reaper under test.
func seedTerminated(t *testing.T, marks map[string]string) (*Manager, *recordingInstanceCleaner, *fakeStateStore, *TerminatedTeardownReaper, *VM) {
	t.Helper()
	store := newFakeStateStore()
	m, cleaner, _, _, _ := terminateTestManager(t, store)
	v := &VM{
		ID:       "i-gc",
		Status:   StateTerminated,
		LastNode: "test-node",
		ENIId:    "eni-gc",
		PublicIP: "203.0.113.9",
		Instance: &ec2.Instance{},
		Teardown: marks,
	}
	require.NoError(t, store.WriteTerminatedInstance(v.ID, v))
	return m, cleaner, store, m.NewTerminatedTeardownReaper(), v
}

// TestRLC5_GCRetriesPendingTeardownToDoneThenPurges enforces ADR-0003 §1/§3
// (retry pending teardown): a terminated record left with outstanding
// pending/failed dependents by an interrupted terminate must be driven to all
// `done` by the GC and then purged — never abandoned.
func TestRLC5_GCRetriesPendingTeardownToDoneThenPurges(t *testing.T) {
	m, cleaner, store, reaper, v := seedTerminated(t, map[string]string{
		TeardownVolumes: string(TeardownFailed),
		TeardownNAT:     string(TeardownPending),
		TeardownENI:     string(TeardownFailed),
		TeardownOVN:     string(TeardownPending),
	})

	reaped, err := reaper.Sweep(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, reaped, "ADR-0003 §3: the GC must complete and purge the record")
	assert.True(t, v.TeardownComplete(), "ADR-0003 §1: every dependent must reach done")
	remaining, err := store.ListTerminatedInstances()
	require.NoError(t, err)
	assert.Empty(t, remaining, "ADR-0003 §3: a record whose teardown the GC completed must be purged")
	// The reaper re-drove the real idempotent cleaner calls.
	assert.Equal(t, []string{v.ID}, cleaner.deleteVolumes)
	assert.Equal(t, []string{v.ID}, cleaner.releasePublicIP)
	assert.Equal(t, []string{v.ID}, cleaner.detachAndDeleteENI)
	_ = m
}

// TestRLC5_GCNeverPurgesIncompleteRecord enforces ADR-0003 §4 (no-orphan
// completeness / never abandon): a dependent that keeps failing must leave the
// terminated record present and marked, not purged, so a later sweep retries it.
func TestRLC5_GCNeverPurgesIncompleteRecord(t *testing.T) {
	m, cleaner, store, reaper, v := seedTerminated(t, map[string]string{
		TeardownVolumes: string(TeardownDone),
		TeardownENI:     string(TeardownFailed),
		TeardownOVN:     string(TeardownPending),
	})
	cleaner.detachENIErr = errors.New("eni delete still failing")

	reaped, err := reaper.Sweep(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 0, reaped, "ADR-0003 §4: a record with a still-failing dependent must not be purged")
	assert.False(t, v.TeardownComplete())
	assert.Equal(t, string(TeardownFailed), v.Teardown[TeardownENI], "a failed retry must stay Failed, not silently drop")
	remaining, err := store.ListTerminatedInstances()
	require.NoError(t, err)
	require.Len(t, remaining, 1, "ADR-0003 §4: an incomplete record must be retained (GC-visible) for the next sweep")
	_ = m
}

// TestRLC5_GCSkipsOtherNodesRecords enforces home-node ownership: a node must
// not run node-local teardown for a record another node owns.
func TestRLC5_GCSkipsOtherNodesRecords(t *testing.T) {
	store := newFakeStateStore()
	m, cleaner, _, _, _ := terminateTestManager(t, store)
	other := &VM{
		ID:       "i-other",
		Status:   StateTerminated,
		LastNode: "some-other-node",
		ENIId:    "eni-other",
		Instance: &ec2.Instance{},
		Teardown: map[string]string{TeardownENI: string(TeardownFailed)},
	}
	require.NoError(t, store.WriteTerminatedInstance(other.ID, other))

	reaped, err := m.NewTerminatedTeardownReaper().Sweep(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 0, reaped)
	assert.Empty(t, cleaner.detachAndDeleteENI, "must not tear down a record another node owns")
	remaining, err := store.ListTerminatedInstances()
	require.NoError(t, err)
	assert.Len(t, remaining, 1)
}

// TestRLC5_GCHoldsWhenKVDegraded enforces ADR-0003 §3 (KV-health gated): while
// KV is unhealthy the GC holds — a completable record is left untouched rather
// than reaped against a desired-state the node cannot trust — and resumes once
// KV is healthy again.
func TestRLC5_GCHoldsWhenKVDegraded(t *testing.T) {
	_, _, store, reaper, _ := seedTerminated(t, map[string]string{
		TeardownVolumes: string(TeardownFailed),
	})

	healthy := false
	gc := NewGarbageCollector(func() bool { return healthy }, reaper)

	gc.SweepOnce(context.Background())
	remaining, err := store.ListTerminatedInstances()
	require.NoError(t, err)
	require.Len(t, remaining, 1, "ADR-0003 §3: GC must hold (not purge) while KV is degraded")

	healthy = true
	gc.SweepOnce(context.Background())
	remaining, err = store.ListTerminatedInstances()
	require.NoError(t, err)
	assert.Empty(t, remaining, "ADR-0003 §3: GC resumes and completes once KV is healthy")
}
