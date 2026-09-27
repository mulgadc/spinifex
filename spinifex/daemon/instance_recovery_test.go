package daemon

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recoveryFixture is the reconciler with no daemon behind it. Selection is the
// whole of the policy and is pure over the records and the liveness answer, so
// none of these tests needs a cluster.
func recoveryFixture(self string) *instanceRecovery {
	return &instanceRecovery{
		daemon:    &Daemon{node: self},
		staleOnce: map[string]struct{}{},
		backoff:   map[string]recoveryBackoff{},
	}
}

func runningOn(id, node string) *vm.InstanceRecord {
	return &vm.InstanceRecord{
		Metadata: resource.Metadata{Name: id, AccountID: "111122223333"},
		Spec:     vm.InstanceSpec{InstanceType: "t3.micro", DesiredState: vm.DesiredRunning},
		Status:   vm.InstanceStatus{Status: vm.StateRunning, LastNode: node},
	}
}

func stale(node string) instancecache.NodeState {
	return instancecache.NodeStale
}

// twice runs two passes, which is what the reconciler requires before it acts.
func twice(r *instanceRecovery, records []*vm.InstanceRecord, state func(string) instancecache.NodeState) []recoveryCandidate {
	r.selectCandidates(records, state)
	return r.selectCandidates(records, state)
}

func TestARecordOwnedByAStaleNodeIsRecovered(t *testing.T) {
	r := recoveryFixture("node-2")
	got := twice(r, []*vm.InstanceRecord{runningOn("i-1", "node-1")}, stale)

	require.Len(t, got, 1)
	assert.Equal(t, "i-1", got[0].record.Metadata.Name)
	assert.Equal(t, "node-1", got[0].from, "the candidate has to name who it is being taken from")
}

// One sample is one node's clock. Liveness compares the timestamp the owner
// wrote into its own heartbeat, so a single stale reading is as likely to be
// skew as a failure.
func TestOneStaleSampleIsNotEnough(t *testing.T) {
	r := recoveryFixture("node-2")
	assert.Empty(t, r.selectCandidates([]*vm.InstanceRecord{runningOn("i-1", "node-1")}, stale))
}

// A node that came back between the two passes must not still be a candidate
// on the strength of the first one.
func TestAnOwnerThatRecoversBetweenPassesIsForgotten(t *testing.T) {
	r := recoveryFixture("node-2")
	records := []*vm.InstanceRecord{runningOn("i-1", "node-1")}

	r.selectCandidates(records, stale)
	assert.Empty(t, r.selectCandidates(records, func(string) instancecache.NodeState {
		return instancecache.NodeLive
	}))
	assert.Empty(t, r.selectCandidates(records, stale),
		"the run of stale passes has to start again, not resume where it left off")
}

// An unreadable heartbeat store is not evidence that a node is gone. Treating
// it as one turns a KV outage into a cluster-wide relaunch.
func TestAnUnknownOwnerIsNeverACandidate(t *testing.T) {
	r := recoveryFixture("node-2")
	got := twice(r, []*vm.InstanceRecord{runningOn("i-1", "node-1")}, func(string) instancecache.NodeState {
		return instancecache.NodeUnknown
	})
	assert.Empty(t, got)
}

func TestThisNodesOwnInstancesAreNeverCandidates(t *testing.T) {
	r := recoveryFixture("node-1")
	assert.Empty(t, twice(r, []*vm.InstanceRecord{runningOn("i-1", "node-1")}, stale))
}

// Desired state is what separates a recovery from a stop. A drain leaves an
// instance stopped but still wanted, and that is a recovery; an operator stop
// leaves it stopped and not wanted, and that is not.
func TestWhatDesiredStateAdmits(t *testing.T) {
	drained := runningOn("i-drained", "node-1")
	drained.Status.Status = vm.StateStopped

	stopped := runningOn("i-stopped", "node-1")
	stopped.Status.Status = vm.StateStopped
	stopped.Spec.DesiredState = vm.DesiredStopped

	terminating := runningOn("i-terminating", "node-1")
	terminating.Status.Status = vm.StateShuttingDown

	deleting := runningOn("i-deleting", "node-1")
	now := time.Now()
	deleting.Metadata.DeletionTimestamp = &now

	r := recoveryFixture("node-2")
	got := twice(r, []*vm.InstanceRecord{drained, stopped, terminating, deleting}, stale)

	require.Len(t, got, 1)
	assert.Equal(t, "i-drained", got[0].record.Metadata.Name,
		"a drain-stopped instance is still wanted running, so it is recovered")
}

func TestARecordWithNoOwnerIsNotACandidate(t *testing.T) {
	r := recoveryFixture("node-2")
	assert.Empty(t, twice(r, []*vm.InstanceRecord{runningOn("i-1", "")}, stale))
}

// A guest paused because its storage stopped answering is not a host failure,
// and it is the one failure a different host cannot fix: the volumes are the
// same objects in the same store from anywhere.
func TestAnInstancePausedOnItsStorageIsNotMoved(t *testing.T) {
	faulted := runningOn("i-faulted", "node-1")
	faulted.Status.Health.IOErrorSince = time.Now().Add(-2 * time.Minute)
	faulted.Status.Health.IOErrorResumes = 4

	healthy := runningOn("i-healthy", "node-1")

	r := recoveryFixture("node-2")
	got := twice(r, []*vm.InstanceRecord{faulted, healthy}, stale)

	require.Len(t, got, 1)
	assert.Equal(t, "i-healthy", got[0].record.Metadata.Name,
		"only the guest whose storage was answering is worth moving")
}

// A guest that had recovered from an earlier pause is an ordinary candidate.
// IOErrorSince clears on the first running poll, so a non-zero value means the
// storage was still failing when the owner last wrote.
func TestAPastStoragePauseThatClearedDoesNotBlockRecovery(t *testing.T) {
	recovered := runningOn("i-1", "node-1")
	recovered.Status.Health.IOErrorResumes = 0

	r := recoveryFixture("node-2")
	assert.Len(t, twice(r, []*vm.InstanceRecord{recovered}, stale), 1)
}

// A launch that cannot complete has to cost less each time it fails. Some
// causes clear in seconds and some need an operator, and nothing here can tell
// which, so the retry rate has to suit the second.
func TestRetriesBackOffAndCapAfterAFailedLaunch(t *testing.T) {
	r := recoveryFixture("node-2")
	now := time.Now()

	assert.False(t, r.backingOff("i-1", now), "an instance never tried is not backed off")

	require.Equal(t, 1, r.deferRetry("i-1", now))
	assert.True(t, r.backingOff("i-1", now.Add(recoveryBackoffBase-time.Second)))
	assert.False(t, r.backingOff("i-1", now.Add(recoveryBackoffBase)))

	require.Equal(t, 2, r.deferRetry("i-1", now))
	assert.True(t, r.backingOff("i-1", now.Add(2*recoveryBackoffBase-time.Second)),
		"the second failure has to wait longer than the first")

	for range 20 {
		r.deferRetry("i-1", now)
	}
	assert.False(t, r.backingOff("i-1", now.Add(recoveryBackoffMax)),
		"the delay is capped, so a fault that is fixed is noticed without an operator nudge")
}
