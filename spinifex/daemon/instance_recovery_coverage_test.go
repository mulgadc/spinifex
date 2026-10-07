//test:in-package — the recovery loop, its pass and its attempt are unexported,
//and these tests drive them against a real store rather than through a cluster.

package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recoveryStore is a JetStreamManager over private record and heartbeat
// buckets on the shared server, with a liveness view of the heartbeats.
type recoveryStore struct {
	m        *JetStreamManager
	liveness *instancecache.Liveness
}

func newRecoveryStore(t *testing.T) *recoveryStore {
	t.Helper()
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	clustersize.RedeclareForTest(t, 1)

	name := fmt.Sprintf("recovery-%d", time.Now().UnixNano())
	m.setInstanceStateBucket(kvstore.NewBucket(m.js, kvstore.Config{Name: name, History: 1}))
	_, err = m.stateB.KV(t.Context())
	require.NoError(t, err)

	beats := kvstore.Config{Name: name + "-hb", History: 1}
	m.clusterKV, err = kvstore.NewBucket(m.js, beats).KV(t.Context())
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = m.js.DeleteKeyValue(context.Background(), name)
		_ = m.js.DeleteKeyValue(context.Background(), name+"-hb")
	})
	return &recoveryStore{m: m, liveness: instancecache.NewLiveness(m.js, beats)}
}

// beat writes node's heartbeat stamped at.
func (s *recoveryStore) beat(t *testing.T, node string, at time.Time) {
	t.Helper()
	require.NoError(t, s.m.WriteHeartbeat(&Heartbeat{Node: node, Timestamp: at.UTC().Format(time.RFC3339)}))
}

// put writes a desired-running record for id owned by node.
func (s *recoveryStore) put(t *testing.T, id, node, instanceType string) {
	t.Helper()
	record := runningOn(id, node)
	record.Spec.InstanceType = instanceType
	record.Status.Instance = &ec2.Instance{InstanceId: aws.String(id)}
	require.NoError(t, s.m.records.Set(context.Background(), instanceRecordKey(id), record))
}

func (s *recoveryStore) load(t *testing.T, id string) *vm.InstanceRecord {
	t.Helper()
	record, err := s.m.LoadInstanceRecord(id)
	require.NoError(t, err)
	require.NotNil(t, record)
	return record
}

// recoveryDaemon is a node-1 daemon whose recovery loop reads s, has already
// settled, and finds its own object store healthy.
func recoveryDaemon(t *testing.T, s *recoveryStore) (*Daemon, *instanceRecovery) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	origTTL := predastoreHealthCacheTTL
	predastoreHealthCacheTTL = time.Hour
	t.Cleanup(func() { predastoreHealthCacheTTL = origTTL })

	d := createTestDaemon(t, sharedNATSURL)
	d.jsManager = s.m
	d.predastoreHealth.at = time.Now()
	d.predastoreHealth.result = predastoreHealthOK

	return d, &instanceRecovery{
		daemon:      d,
		liveness:    s.liveness,
		startedAt:   time.Now(),
		settledOnce: true,
		staleOnce:   map[string]struct{}{},
		backoff:     map[string]recoveryBackoff{},
	}
}

// schedulableType is an instance type this daemon can allocate.
func schedulableType(t *testing.T, d *Daemon) string {
	t.Helper()
	for name := range d.resourceMgr.instanceTypes {
		if strings.HasSuffix(name, ".nano") || strings.HasSuffix(name, ".micro") {
			return name
		}
	}
	t.Fatal("no small instance type on this host")
	return ""
}

func threeNodes() *config.ClusterConfig {
	return &config.ClusterConfig{Nodes: map[string]config.Config{"node-1": {}, "node-2": {}, "node-3": {}}}
}

func TestRecoveryStartsAndStopsOnContext(t *testing.T) {
	t.Run("no liveness view means no loop", func(t *testing.T) {
		logs := captureSlogForTest(t)
		d := &Daemon{
			node:            "node-1",
			clusterConfig:   threeNodes(),
			jsManager:       &JetStreamManager{},
			instanceService: &handlers_ec2_instance.InstanceServiceImpl{},
		}
		d.startInstanceRecovery()
		d.shutdownWg.Wait()
		assert.Contains(t, logs.String(), "no liveness view")
	})

	t.Run("the loop runs until the daemon stops", func(t *testing.T) {
		logs := captureSlogForTest(t)
		nc, err := nats.Connect(sharedJSNATSURL)
		require.NoError(t, err)
		t.Cleanup(nc.Close)
		m, err := NewJetStreamManager(nc)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		d := &Daemon{
			node:            "node-1",
			clusterConfig:   threeNodes(),
			jsManager:       m,
			instanceService: &handlers_ec2_instance.InstanceServiceImpl{},
			ctx:             ctx,
			cancel:          cancel,
		}
		d.startInstanceRecovery()
		assert.Contains(t, logs.String(), "Instance recovery started")

		cancel()
		d.shutdownWg.Wait()
		assert.Contains(t, logs.String(), "Instance recovery stopping",
			"the loop has to exit with the daemon, not outlive the store it reads")
	})
}

func TestRecoveryPassPaths(t *testing.T) {
	t.Run("an unreadable record store ends the pass", func(t *testing.T) {
		logs := captureSlogForTest(t)
		r := recoveryFixture("node-1")
		r.settledOnce = true
		r.daemon.jsManager = &JetStreamManager{}
		r.daemon.predastoreHealth.at = time.Now()
		r.daemon.predastoreHealth.result = predastoreHealthOK

		r.pass(context.Background())
		assert.Contains(t, logs.String(), "could not read the instance records")
	})

	t.Run("a backed-off candidate is left where it is", func(t *testing.T) {
		s := newRecoveryStore(t)
		_, r := recoveryDaemon(t, s)
		s.beat(t, "node-dead", time.Now().Add(-time.Hour))
		s.put(t, "i-backoff", "node-dead", "zz9.none")
		r.backoff["i-backoff"] = recoveryBackoff{until: time.Now().Add(time.Hour)}

		r.pass(t.Context())
		r.pass(t.Context())

		assert.Equal(t, "node-dead", s.load(t, "i-backoff").Status.LastNode, "a backed-off instance must not be claimed")
		assert.Zero(t, r.backoff["i-backoff"].attempts)
	})

	t.Run("a cancelled pass stops before claiming", func(t *testing.T) {
		s := newRecoveryStore(t)
		_, r := recoveryDaemon(t, s)
		s.beat(t, "node-dead", time.Now().Add(-time.Hour))
		s.put(t, "i-cancel", "node-dead", "zz9.none")

		r.pass(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r.pass(ctx)

		assert.Contains(t, r.staleOnce, "i-cancel", "the second pass derived the candidate from the memoised liveness")
		assert.Equal(t, "node-dead", s.load(t, "i-cancel").Status.LastNode, "a cancelled pass must not claim")
		assert.Empty(t, r.backoff)
	})

	t.Run("a candidate seen stale twice is attempted", func(t *testing.T) {
		s := newRecoveryStore(t)
		_, r := recoveryDaemon(t, s)
		s.beat(t, "node-dead", time.Now().Add(-time.Hour))
		s.put(t, "i-attempt", "node-dead", "zz9.none")

		r.pass(t.Context())
		assert.Empty(t, r.backoff, "one stale sample is not enough to act on")
		r.pass(t.Context())

		assert.Equal(t, 1, r.backoff["i-attempt"].attempts, "the failed launch counts against the budget")
		assert.Equal(t, "node-dead", s.load(t, "i-attempt").Status.LastNode,
			"a launch that could not run here hands the record back")
	})
}

func TestRecoveryAttemptFailureAndBudget(t *testing.T) {
	t.Run("an unschedulable type is handed back and backed off", func(t *testing.T) {
		s := newRecoveryStore(t)
		_, r := recoveryDaemon(t, s)
		s.put(t, "i-nofit", "node-dead", "zz9.none")

		r.attempt(t.Context(), recoveryCandidate{record: s.load(t, "i-nofit"), from: "node-dead"})

		record := s.load(t, "i-nofit")
		assert.Equal(t, "node-dead", record.Status.LastNode)
		assert.Equal(t, vm.DesiredRunning, record.Spec.DesiredState, "one failure is not a give-up")
		held := r.backoff["i-nofit"]
		assert.Equal(t, 1, held.attempts)
		assert.True(t, held.until.After(time.Now()))
	})

	t.Run("the last attempt in the budget stops the instance with the reason", func(t *testing.T) {
		s := newRecoveryStore(t)
		_, r := recoveryDaemon(t, s)
		s.put(t, "i-budget", "node-dead", "zz9.none")
		r.backoff["i-budget"] = recoveryBackoff{attempts: recoveryMaxAttempts - 1}
		r.staleOnce["i-budget"] = struct{}{}

		r.attempt(t.Context(), recoveryCandidate{record: s.load(t, "i-budget"), from: "node-dead"})

		record := s.load(t, "i-budget")
		assert.Equal(t, vm.DesiredStopped, record.Spec.DesiredState)
		assert.Equal(t, vm.StateStopped, record.Status.Status)
		require.NotNil(t, record.Status.Instance.StateReason)
		assert.Equal(t, "Server.InsufficientInstanceCapacity", aws.StringValue(record.Status.Instance.StateReason.Code))
		assert.NotContains(t, r.backoff, "i-budget")
		assert.NotContains(t, r.staleOnce, "i-budget")
	})
}

func TestRecoveryAttemptSucceeds(t *testing.T) {
	s := newRecoveryStore(t)
	d, r := recoveryDaemon(t, s)

	// Marking the guest as terminating before Run makes launch return without
	// starting QEMU, which is the one part of a recovery this cannot do.
	d.vmMgr.SetDeps(vm.Deps{
		NodeID:             d.node,
		VolumeMounter:      newVolumeMounterAdapter(d.natsConn, d.node, d.volumeService),
		VolumeStateUpdater: d.volumeService,
		Hooks: vm.ManagerHooks{BeforeInstanceRelaunch: func(v *vm.VM) error {
			d.vmMgr.Inspect(v, func(v *vm.VM) { v.Status = vm.StateShuttingDown })
			return nil
		}},
	})

	s.put(t, "i-recovered", "node-dead", schedulableType(t, d))
	r.backoff["i-recovered"] = recoveryBackoff{attempts: 2}
	r.staleOnce["i-recovered"] = struct{}{}

	r.attempt(t.Context(), recoveryCandidate{record: s.load(t, "i-recovered"), from: "node-dead"})

	assert.Equal(t, "node-1", s.load(t, "i-recovered").Status.LastNode, "the claim stands once the launch succeeds")
	assertVMPresent(t, d, "i-recovered")
	assert.NotContains(t, r.backoff, "i-recovered", "a recovered instance starts a fresh budget")
	assert.NotContains(t, r.staleOnce, "i-recovered")
}

func TestAbandonOutcomes(t *testing.T) {
	cause := fmt.Errorf("launch refused")

	t.Run("a give-up that cannot be written keeps the backoff", func(t *testing.T) {
		r := recoveryFixture("node-1")
		r.daemon.jsManager = &JetStreamManager{}
		r.backoff["i-1"] = recoveryBackoff{attempts: recoveryMaxAttempts}

		r.abandon(t.Context(), "i-1", "node-dead", recoveryFaultUnknown, cause, recoveryMaxAttempts)
		assert.Contains(t, r.backoff, "i-1", "the next pass retries the launch rather than the give-up")
	})

	t.Run("an instance another node now owns is left running", func(t *testing.T) {
		s := newRecoveryStore(t)
		r := recoveryFixture("node-1")
		r.daemon.jsManager = s.m
		s.put(t, "i-moved", "node-3", "t3.micro")
		r.backoff["i-moved"] = recoveryBackoff{attempts: recoveryMaxAttempts}
		r.staleOnce["i-moved"] = struct{}{}

		r.abandon(t.Context(), "i-moved", "node-dead", recoveryFaultUnknown, cause, recoveryMaxAttempts)

		record := s.load(t, "i-moved")
		assert.Equal(t, vm.DesiredRunning, record.Spec.DesiredState)
		assert.Equal(t, "node-3", record.Status.LastNode)
		assert.NotContains(t, r.backoff, "i-moved")
		assert.NotContains(t, r.staleOnce, "i-moved")
	})

	t.Run("an instance still on its dead owner is stopped with the reason", func(t *testing.T) {
		s := newRecoveryStore(t)
		r := recoveryFixture("node-1")
		r.daemon.jsManager = s.m
		s.put(t, "i-gone", "node-dead", "t3.micro")
		r.backoff["i-gone"] = recoveryBackoff{attempts: recoveryMaxAttempts}

		r.abandon(t.Context(), "i-gone", "node-dead", recoveryFaultUnknown, cause, recoveryMaxAttempts)

		record := s.load(t, "i-gone")
		assert.Equal(t, vm.DesiredStopped, record.Spec.DesiredState)
		reason := record.Status.Instance.StateReason
		require.NotNil(t, reason)
		assert.Equal(t, "Server.HostRecoveryFailed", aws.StringValue(reason.Code))
		assert.Contains(t, aws.StringValue(reason.Message), "failed 12 times")
		assert.NotContains(t, r.backoff, "i-gone")
	})
}

// The fault name is what the span and the give-up log report, so each class
// has to keep a distinct label.
func TestRecoveryFaultString(t *testing.T) {
	assert.Equal(t, "volume_lease_held", recoveryFaultLeaseHeld.String())
	assert.Equal(t, "insufficient_capacity", recoveryFaultCapacity.String())
	assert.Equal(t, "unknown", recoveryFaultUnknown.String())
	assert.Equal(t, "unknown", recoveryFault(99).String())
}

// Every survivor derives the same candidates, so each has to try them in an
// order of its own or they all race the same instance first.
func TestCandidateOrderingIsPerNode(t *testing.T) {
	var records []*vm.InstanceRecord
	for i := range 8 {
		records = append(records, runningOn(fmt.Sprintf("i-%d", i), "node-dead"))
	}
	order := func(self string) []string {
		got := twice(recoveryFixture(self), records, stale)
		ids := make([]string, len(got))
		for i, c := range got {
			ids[i] = c.record.Metadata.Name
		}
		return ids
	}

	a, b := order("node-1"), order("node-2")
	require.Len(t, a, len(records))
	assert.Equal(t, a, order("node-1"), "one node's order is stable, so a failure is reproducible")
	assert.ElementsMatch(t, a, b)
	assert.NotEqual(t, a, b, "two survivors must not share one order")

	r := recoveryFixture("node-1")
	assert.True(t, slices.IsSortedFunc(a, func(x, y string) int {
		return cmp.Compare(r.preference(x), r.preference(y))
	}), "candidates are tried in this node's preference order")
}

func TestNewLiveness(t *testing.T) {
	assert.Nil(t, (&Daemon{}).newLiveness(), "no JetStream manager")
	assert.Nil(t, (&Daemon{jsManager: &JetStreamManager{}}).newLiveness(), "a manager with no JetStream")

	s := newRecoveryStore(t)
	assert.NotNil(t, (&Daemon{jsManager: s.m}).newLiveness())
}

// A returning node drops only the instances the records say another node now
// runs. Its own, ones with no record, and ones it cannot read are kept.
func TestForgetSupersededInstances(t *testing.T) {
	s := newRecoveryStore(t)
	d, _ := recoveryDaemon(t, s)

	s.put(t, "i-moved", "node-3", "")
	s.put(t, "i-mine", "node-1", "")
	kv, err := s.m.stateB.KV(t.Context())
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), instanceRecordKey("i-corrupt"), []byte("{"))
	require.NoError(t, err)

	for _, id := range []string{"i-moved", "i-mine", "i-corrupt", "i-unrecorded"} {
		d.vmMgr.Insert(&vm.VM{ID: id, Status: vm.StateStopped})
	}

	d.forgetSupersededInstances()

	assertVMNotPresent(t, d, "i-moved", "another node owns it now")
	assertVMPresent(t, d, "i-mine")
	assertVMPresent(t, d, "i-corrupt", "an unreadable record is not evidence the instance moved")
	assertVMPresent(t, d, "i-unrecorded")
}
