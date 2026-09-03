//test:in-package — the sweep's ticker interval and its Daemon wiring are both unexported seams
package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRecordManagerInPackage is the in-package twin of the daemon_test fixture,
// needed here because these tests build a Daemon out of unexported fields.
func newRecordManagerInPackage(t *testing.T) *JetStreamManager {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	m, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())
	require.NoError(t, m.InitTerminatedInstanceBucket())
	return m
}

// recordRevisionInPackage reads a record's KV revision, so a test can tell a
// sweep that wrote nothing from one that rewrote the same bytes.
func recordRevisionInPackage(t *testing.T, m *JetStreamManager, instanceID string) uint64 {
	t.Helper()
	_, revision, err := m.records.Get(context.Background(), instanceRecordKey(instanceID))
	require.NoError(t, err)
	return revision
}

// The interval is the bound this whole phase exists to establish, so it is
// asserted directly rather than inferred from a sweep.
func TestRecordRepairInterval_IsTheDocumentedBound(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 60*time.Second, recordRepairInterval)
}

func TestStartRecordRepair_NoJetStreamIsNotFatal(t *testing.T) {
	t.Parallel()
	d := &Daemon{ctx: t.Context(), config: &config.Config{}}
	d.startRecordRepair()
	d.shutdownWg.Wait()
}

// The sweep must stop with the daemon rather than outliving it, since it writes
// to a store the shutdown is tearing down.
func TestStartRecordRepair_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	m := newRecordManagerInPackage(t)

	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{
		ctx:                  ctx,
		node:                 "node-1",
		config:               &config.Config{AZ: "us-east-1a"},
		jsManager:            m,
		vmMgr:                vm.NewManager(),
		recordRepairInterval: time.Millisecond,
	}
	d.startRecordRepair()

	cancel()
	done := make(chan struct{})
	go func() { d.shutdownWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the repair sweep did not stop when the daemon context was cancelled")
	}
}

// The sweep republishes a record whose write failed without the instance
// changing, which is the whole guarantee: an instance that launches, fails its
// write and then simply runs still gets a record.
func TestRepairInstanceRecords_RepublishesAFailedWriteWithNoStateChange(t *testing.T) {
	t.Parallel()
	m := newRecordManagerInPackage(t)
	require.NoError(t, m.WriteNodeMarker("node-1"))

	// The state a failed write leaves behind: no record on the key space and no
	// digest entry claiming there is one. WriteRunningSet drops the digest entry
	// on failure precisely so this is what a later reconcile sees.
	instance := &vm.VM{ID: "i-1", InstanceType: "t3.nano", Status: vm.StateRunning, LastNode: "node-1"}
	record, err := loadRecord(m.records, instanceRecordKey("i-1"))
	require.NoError(t, err)
	require.Nil(t, record)

	d := &Daemon{
		ctx: context.Background(), node: "node-1",
		config: &config.Config{AZ: "us-east-1a"}, jsManager: m, vmMgr: vm.NewManager(),
	}
	d.vmMgr.Insert(instance)

	d.repairInstanceRecords()

	record, err = loadRecord(m.records, instanceRecordKey("i-1"))
	require.NoError(t, err)
	require.NotNil(t, record, "the sweep should have published the record the failed write dropped")
	assert.Equal(t, "node-1", record.Status.LastNode)
	assert.Equal(t, "us-east-1a", record.Status.AZ)
}

// A node with nothing outstanding must not turn the sweep into traffic: the
// digest guard is what keeps the cost proportional to failures.
func TestRepairInstanceRecords_HealthyNodeWritesNothing(t *testing.T) {
	t.Parallel()
	m := newRecordManagerInPackage(t)
	require.NoError(t, m.WriteNodeMarker("node-1"))

	instance := &vm.VM{ID: "i-1", InstanceType: "t3.nano", Status: vm.StateRunning, LastNode: "node-1"}
	first := m.WriteRunningSet("node-1", "us-east-1a", map[string]*vm.VM{instance.ID: instance})
	require.Equal(t, 1, first.Written)

	d := &Daemon{
		ctx: context.Background(), node: "node-1",
		config: &config.Config{AZ: "us-east-1a"}, jsManager: m, vmMgr: vm.NewManager(),
	}
	d.vmMgr.Insert(instance)

	before := recordRevisionInPackage(t, m, "i-1")
	d.repairInstanceRecords()
	assert.Equal(t, before, recordRevisionInPackage(t, m, "i-1"),
		"a sweep over an already-published set must not write")
}

// The QMP collector advances LastQMPSuccess on its own cadence, so digesting it
// made every running instance look changed once a minute and turned the sweep
// into permanent cluster-wide write traffic. Caught on env19, not here.
func TestRepairInstanceRecords_LivenessStampAloneDoesNotWrite(t *testing.T) {
	t.Parallel()
	m := newRecordManagerInPackage(t)
	require.NoError(t, m.WriteNodeMarker("node-1"))

	instance := &vm.VM{ID: "i-1", InstanceType: "t3.nano", Status: vm.StateRunning, LastNode: "node-1"}
	instance.Health.LastQMPSuccess = time.Now()

	d := &Daemon{
		ctx: context.Background(), node: "node-1",
		config: &config.Config{AZ: "us-east-1a"}, jsManager: m, vmMgr: vm.NewManager(),
	}
	d.vmMgr.Insert(instance)
	d.repairInstanceRecords()
	before := recordRevisionInPackage(t, m, "i-1")

	for i := range 3 {
		d.vmMgr.UpdateState("i-1", func(v *vm.VM) {
			v.Health.LastQMPSuccess = time.Now().Add(time.Duration(i+1) * time.Minute)
		})
		d.repairInstanceRecords()
	}

	assert.Equal(t, before, recordRevisionInPackage(t, m, "i-1"),
		"a moving liveness stamp is not a state change and must not be republished")
}

// The exclusion must be narrow: an impairment is a real transition and has to
// reach the record space, or a describe cannot see it.
func TestRepairInstanceRecords_RealHealthChangeStillWrites(t *testing.T) {
	t.Parallel()
	m := newRecordManagerInPackage(t)
	require.NoError(t, m.WriteNodeMarker("node-1"))

	instance := &vm.VM{ID: "i-1", InstanceType: "t3.nano", Status: vm.StateRunning, LastNode: "node-1"}
	d := &Daemon{
		ctx: context.Background(), node: "node-1",
		config: &config.Config{AZ: "us-east-1a"}, jsManager: m, vmMgr: vm.NewManager(),
	}
	d.vmMgr.Insert(instance)
	d.repairInstanceRecords()
	before := recordRevisionInPackage(t, m, "i-1")

	d.vmMgr.UpdateState("i-1", func(v *vm.VM) {
		v.Health.QMPConsecutiveFailures = 3
		v.Health.ImpairedSince = time.Now()
	})
	d.repairInstanceRecords()

	assert.Greater(t, recordRevisionInPackage(t, m, "i-1"), before,
		"an impairment must still be published")
}
