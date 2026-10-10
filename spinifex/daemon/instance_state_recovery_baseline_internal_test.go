package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/resource"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests characterise what a returning node does with its local file
// against the shared record today. They pin current behaviour, including the
// cases ADR-0007 S5 rules out, so the change to them is visible.

// rebootWithLocalFile empties the in-memory set and reloads it from the local
// file, which is the start of the real boot order.
func rebootWithLocalFile(t *testing.T, d *Daemon) {
	t.Helper()
	d.vmMgr.Replace(map[string]*vm.VM{})
	require.NoError(t, d.LoadState())
	require.NoError(t, d.jsManager.WriteShutdownMarker(d.node))
}

func instanceRecordAt(t *testing.T, d *Daemon, id string) *vm.InstanceRecord {
	t.Helper()
	record, err := d.jsManager.LoadInstanceRecord(id)
	require.NoError(t, err)
	return record
}

// Current behaviour: the local copy is kept, spends a restart on a relaunch, and
// the next state write recreates the shared record from it, owned by this node.
func TestCurrentBehaviour_RestoreMissingCanonicalKeyRepublishesFromLocal(t *testing.T) {
	d := createDaemonWithJetStream(t)
	d.vmMgr.Insert(&vm.VM{ID: "i-nokey", Status: vm.StateError, InstanceType: "t3.micro"})
	require.NoError(t, d.WriteState())
	require.NoError(t, d.jsManager.DeleteInstanceRecord("i-nokey"))
	require.Nil(t, instanceRecordAt(t, d, "i-nokey"), "precondition: the marker stays, the record is gone")

	rebootWithLocalFile(t, d)
	require.NoError(t, d.restoreInstances())

	d.vmMgr.WaitForBackgroundWork()
	instance, ok := d.vmMgr.Get("i-nokey")
	require.True(t, ok, "CURRENT: a missing canonical record does not remove the local instance")
	assert.Equal(t, 1, instance.Health.RestartCount, "CURRENT: a relaunch was attempted from the local copy")
	record := instanceRecordAt(t, d, "i-nokey")
	require.NotNil(t, record, "CURRENT: the record is recreated from the local file")
	assert.Equal(t, d.node, record.Status.LastNode)
}

// Current behaviour: the terminated bucket is not consulted on restore, so an
// instance whose live record is gone and whose terminated record exists is
// revived from the local file and republished as live.
func TestCurrentBehaviour_RestoreIgnoresTerminatedBucket(t *testing.T) {
	d := createDaemonWithJetStream(t)
	d.vmMgr.Insert(&vm.VM{ID: "i-gone", Status: vm.StateError, InstanceType: "t3.micro"})
	require.NoError(t, d.WriteState())
	require.NoError(t, d.jsManager.DeleteInstanceRecord("i-gone"))
	require.NoError(t, d.jsManager.WriteTerminatedInstance("i-gone",
		&vm.VM{ID: "i-gone", Status: vm.StateTerminated, LastNode: d.node}))

	rebootWithLocalFile(t, d)
	require.NoError(t, d.restoreInstances())

	assertVMPresent(t, d, "i-gone", "CURRENT: a terminated record elsewhere does not stop the local copy")
	record := instanceRecordAt(t, d, "i-gone")
	require.NotNil(t, record, "CURRENT: republished into the live key space")
	assert.NotEqual(t, vm.StateTerminated, record.Status.Status)
}

// Current behaviour: the local copy is dropped and the other node's record is
// left untouched, neither restarted here nor overwritten.
func TestCurrentBehaviour_RestoreRecordOwnedElsewhereDropsLocal(t *testing.T) {
	d := createDaemonWithJetStream(t)
	d.vmMgr.Insert(&vm.VM{ID: "i-moved", Status: vm.StateError, InstanceType: "t3.micro"})
	require.NoError(t, d.WriteState())
	_, err := d.jsManager.UpdateInstanceRecord("i-moved", func(r *vm.InstanceRecord) {
		r.Status.LastNode = "node-2"
		r.Status.Status = vm.StateRunning
	})
	require.NoError(t, err)

	rebootWithLocalFile(t, d)
	require.NoError(t, d.restoreInstances())

	assertVMNotPresent(t, d, "i-moved", "CURRENT: superseded local copy is forgotten")
	record := instanceRecordAt(t, d, "i-moved")
	require.NotNil(t, record)
	assert.Equal(t, "node-2", record.Status.LastNode, "CURRENT: the owner's record is not overwritten")
	assert.Equal(t, vm.StateRunning, record.Status.Status)

	state, err := ReadLocalState(d.localStatePath())
	require.NoError(t, err)
	assert.NotContains(t, state.VMS, "i-moved", "CURRENT: the local file is rewritten without it")
}

// Current behaviour: a terminal record this node owns replaces the local copy
// in the merge, and classification migrates it out rather than restarting it.
func TestCurrentBehaviour_RestoreTerminalCanonicalRecordRetiresLocal(t *testing.T) {
	d := createDaemonWithJetStream(t)
	d.vmMgr.Insert(&vm.VM{ID: "i-term", Status: vm.StateError, InstanceType: "t3.micro"})
	require.NoError(t, d.WriteState())
	_, err := d.jsManager.UpdateInstanceRecord("i-term", func(r *vm.InstanceRecord) {
		r.Status.Status = vm.StateTerminated
		r.Metadata = resource.Metadata{Name: "i-term"}
	})
	require.NoError(t, err)

	rebootWithLocalFile(t, d)
	require.NoError(t, d.restoreInstances())

	assertVMNotPresent(t, d, "i-term", "CURRENT: the canonical terminal record wins the merge")
	terminated, err := d.jsManager.LoadTerminatedInstance("i-term")
	require.NoError(t, err)
	assert.NotNil(t, terminated, "CURRENT: migrated to the terminated bucket")
}

// Current behaviour: with KV unreachable, restore stops after the failed
// cluster read. Local instances stay in memory as the file left them, are
// neither reset nor relaunched, and the local file is rewritten unchanged.
func TestCurrentBehaviour_RestoreWithKVUnavailableLeavesLocalUnlaunched(t *testing.T) {
	d := createDaemonWithJetStream(t)
	d.vmMgr.Insert(&vm.VM{ID: "i-kvdown", Status: vm.StateRunning, InstanceType: "t3.micro"})
	require.NoError(t, d.WriteState())

	d.vmMgr.Replace(map[string]*vm.VM{})
	require.NoError(t, d.LoadState())
	d.vmMgr.SetDeps(vm.Deps{
		NodeID:                     d.node,
		StateStore:                 d.stateStore,
		TransitionState:            d.TransitionState,
		InstanceTypes:              newInstanceTypeResolverAdapter(d.resourceMgr),
		Resources:                  newResourceControllerAdapter(d.resourceMgr),
		ConsumeCleanShutdownMarker: func() bool { return true },
	})
	d.natsConn.Close()

	require.NoError(t, d.restoreInstances())

	instance, ok := d.vmMgr.Get("i-kvdown")
	require.True(t, ok, "CURRENT: kept")
	assert.Equal(t, vm.StateRunning, instance.Status, "CURRENT: not reset to pending, so not relaunched")

	state, err := ReadLocalState(d.localStatePath())
	require.NoError(t, err)
	require.Contains(t, state.VMS, "i-kvdown")
	assert.Equal(t, vm.StateRunning, state.VMS["i-kvdown"].Status)
}
