//test:in-package — selectCandidates, settled and the backoff are the whole of
//the policy and are deliberately unexported. Exporting them to test them would
//make the reconciler's internals part of the daemon's API for no other reason.

package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
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

// The settle window asks whether this node is still coming up alongside its
// peers, which is answered once. Asked afresh every pass it would become
// "are the peers live now" and refuse to act on the failure it exists for.
func TestTheSettleWindowLatchesOnceThePeersHaveBeenSeen(t *testing.T) {
	r := recoveryFixture("node-1")
	r.startedAt = time.Now()
	r.daemon.clusterConfig = &config.ClusterConfig{
		Nodes: map[string]config.Config{"node-1": {}, "node-2": {}, "node-3": {}},
	}

	live := map[string]instancecache.NodeState{
		"node-2": instancecache.NodeLive, "node-3": instancecache.NodeLive,
	}
	state := func(node string) instancecache.NodeState { return live[node] }
	require.True(t, r.settled(state), "every peer is live, so the cluster is not mid-restart")

	live["node-2"] = instancecache.NodeStale
	assert.True(t, r.settled(state),
		"a peer going away afterwards is the failure to recover from, not a reason to stand down")
}

func TestTheSettleWindowHoldsWhileAPeerHasNotBeenSeen(t *testing.T) {
	r := recoveryFixture("node-1")
	r.startedAt = time.Now()
	r.daemon.clusterConfig = &config.ClusterConfig{
		Nodes: map[string]config.Config{"node-1": {}, "node-2": {}},
	}
	assert.False(t, r.settled(stale),
		"a node restarted alongside its peers must not recover their guests while they boot")
}

func TestTheSettleWindowReleasesWhenItExpires(t *testing.T) {
	r := recoveryFixture("node-1")
	r.startedAt = time.Now().Add(-recoverySettleWindow - time.Second)
	r.daemon.clusterConfig = &config.ClusterConfig{
		Nodes: map[string]config.Config{"node-1": {}, "node-2": {}},
	}
	assert.True(t, r.settled(stale),
		"a peer that never returns is the genuine failure, with nothing to compare against")
}

// Every refusal that stops the loop starting, and none of them is a setting.
// Each is a fact about the cluster — not wired up, or not big enough for
// recovery to mean anything — so no cluster can be configured into leaving a
// dead node's guests down.
func TestWhatStopsTheLoopStarting(t *testing.T) {
	threeNodes := map[string]config.Config{"node-1": {}, "node-2": {}, "node-3": {}}

	tests := []struct {
		name   string
		reason string
		build  func() *Daemon
	}{
		{"no cluster config to read the node list from", "no cluster config", func() *Daemon {
			return &Daemon{node: "node-1"}
		}},
		{"no JetStream to read records from", "JetStream or the instance service is unavailable", func() *Daemon {
			return &Daemon{node: "node-1", clusterConfig: &config.ClusterConfig{Nodes: threeNodes}}
		}},
		{"too few nodes to move a guest between", "inert on a cluster this small", func() *Daemon {
			return &Daemon{
				node:            "node-1",
				clusterConfig:   &config.ClusterConfig{Nodes: map[string]config.Config{"node-1": {}, "node-2": {}}},
				jsManager:       &JetStreamManager{},
				instanceService: &handlers_ec2_instance.InstanceServiceImpl{},
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			d := tc.build()
			require.NotPanics(t, d.startInstanceRecovery,
				"a refusal to start has to return, not fail")
			assert.Contains(t, logs.String(), tc.reason,
				"the operator has to be told which condition made recovery inert here")
		})
	}
}

// TestRecoveryHasNoOffSwitch pins the absence itself, because the cheapest way
// to reintroduce one is to add a field nobody notices. A guest whose host is
// gone is down either way, so a switch could only buy leaving it down.
func TestRecoveryHasNoOffSwitch(t *testing.T) {
	for field := range reflect.TypeFor[config.ClusterConfig]().Fields() {
		assert.NotContains(t, strings.ToLower(field.Name), "recover",
			"ClusterConfig.%s reads as a recovery setting, and recovery is not configurable", field.Name)
	}
}

// The difference between a store broken here and a store broken everywhere is
// made by every node asking only about itself. A node that cannot serve objects
// cannot mount a volume, so claiming one would take an instance off its dead
// owner only to fail, and hold it away from a survivor that could have run it.
func TestANodeWhoseObjectStoreIsDownClaimsNothing(t *testing.T) {
	for _, verdict := range []string{predastoreHealthUnreachable, predastoreHealthNoLeader} {
		t.Run(verdict, func(t *testing.T) {
			r := recoveryFixture("node-1")
			r.daemon.predastoreHealth.at = time.Now()
			r.daemon.predastoreHealth.result = verdict

			assert.False(t, r.canHostRecoveries(context.Background()))
		})
	}
}

func TestANodeWhoseObjectStoreIsHealthyMayClaim(t *testing.T) {
	r := recoveryFixture("node-1")
	r.daemon.predastoreHealth.at = time.Now()
	r.daemon.predastoreHealth.result = predastoreHealthOK

	assert.True(t, r.canHostRecoveries(context.Background()))
}

// A pass with nothing wired behind it must return rather than fail. The store
// verdict is read before the records are, so a node standing down never reaches
// the reader at all.
func TestAPassStandsDownBeforeReadingAnything(t *testing.T) {
	r := recoveryFixture("node-1")
	r.settledOnce = true
	r.daemon.predastoreHealth.at = time.Now()
	r.daemon.predastoreHealth.result = predastoreHealthUnreachable

	assert.NotPanics(t, func() { r.pass(context.Background()) },
		"jsManager is nil here, so reaching the record read would panic")
}

// An unreadable record is not evidence that an instance moved, so a returning
// node with no way to ask leaves its local copies alone.
func TestForgettingSupersededInstancesNeedsAStoreToAsk(t *testing.T) {
	d := &Daemon{node: "node-1"}
	assert.NotPanics(t, d.forgetSupersededInstances)
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
