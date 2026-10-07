package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runningSetOf(ids ...string) map[string]*vm.VM {
	set := make(map[string]*vm.VM, len(ids))
	for _, id := range ids {
		set[id] = &vm.VM{ID: id, InstanceType: "t3.micro", Status: vm.StateRunning}
	}
	return set
}

func TestWriteRunningSet_SeedFailure(t *testing.T) {
	m, f := newFaultInjectedJSM(t)
	f.setFail("ListKeys", true)
	f.setFail("Keys", true)

	got := m.WriteRunningSet("node-1", "", runningSetOf("i-1"))
	assert.Equal(t, RunningSetResult{Failed: 1}, got)

	f.setFail("ListKeys", false)
	f.setFail("Keys", false)
	got = m.WriteRunningSet("node-1", "", runningSetOf("i-1"))
	assert.Equal(t, RunningSetResult{Written: 1}, got, "the next write must seed and publish")
}

func TestWriteRunningSet_NilMemberIsSkipped(t *testing.T) {
	m, _ := newFaultInjectedJSM(t)
	set := runningSetOf("i-1")
	set["i-nil"] = nil

	assert.Equal(t, RunningSetResult{Written: 1}, m.WriteRunningSet("node-1", "", set))
	rec, err := m.LoadInstanceRecord("i-nil")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

// A failed publish forgets its digest, so an unchanged set retries the write.
func TestWriteRunningSet_FailedWriteIsRetried(t *testing.T) {
	m, f := newFaultInjectedJSM(t)
	f.setFail("Create", true)
	assert.Equal(t, RunningSetResult{Failed: 1}, m.WriteRunningSet("node-1", "", runningSetOf("i-1")))

	f.setFail("Create", false)
	assert.Equal(t, RunningSetResult{Written: 1}, m.WriteRunningSet("node-1", "", runningSetOf("i-1")))
}

func TestWriteRunningSet_Retirement(t *testing.T) {
	t.Run("record already gone", func(t *testing.T) {
		m, _ := newFaultInjectedJSM(t)
		m.WriteRunningSet("node-1", "", runningSetOf("i-1"))
		require.NoError(t, m.DeleteStoppedInstance("i-1"))

		assert.Equal(t, RunningSetResult{Retired: 1}, m.WriteRunningSet("node-1", "", nil))
	})

	t.Run("record moved to another node is left alone", func(t *testing.T) {
		m, _ := newFaultInjectedJSM(t)
		m.WriteRunningSet("node-1", "", runningSetOf("i-1"))
		_, err := m.UpdateInstanceRecord("i-1", func(r *vm.InstanceRecord) { r.Status.LastNode = "node-2" })
		require.NoError(t, err)

		assert.Equal(t, RunningSetResult{Retired: 1}, m.WriteRunningSet("node-1", "", nil))
		rec, err := m.LoadInstanceRecord("i-1")
		require.NoError(t, err)
		require.NotNil(t, rec, "the new owner's record must survive the old owner's write")
		assert.Equal(t, "node-2", rec.Status.LastNode)
	})

	t.Run("unreadable record is kept until it can be checked", func(t *testing.T) {
		m, f := newFaultInjectedJSM(t)
		m.WriteRunningSet("node-1", "", runningSetOf("i-1"))

		f.setFail("Get", true)
		assert.Equal(t, RunningSetResult{Failed: 1}, m.WriteRunningSet("node-1", "", nil))
		f.setFail("Get", false)

		assert.Equal(t, RunningSetResult{Retired: 1}, m.WriteRunningSet("node-1", "", nil),
			"the failed retirement must be retried, not forgotten")
	})

	t.Run("delete failure keeps the record", func(t *testing.T) {
		m, f := newFaultInjectedJSM(t)
		m.WriteRunningSet("node-1", "", runningSetOf("i-1"))

		f.setFail("Delete", true)
		assert.Equal(t, RunningSetResult{Failed: 1}, m.WriteRunningSet("node-1", "", nil))
		f.setFail("Delete", false)

		rec, err := m.LoadInstanceRecord("i-1")
		require.NoError(t, err)
		assert.NotNil(t, rec)
	})
}
