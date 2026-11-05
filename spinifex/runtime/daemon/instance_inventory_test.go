package daemon_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The strict inventory takes existence from this listing, so it has to return
// the records the stopped listing leaves out and only drop terminated ones.
func TestListNonTerminatedInstances_ReturnsEveryRecordButTerminated(t *testing.T) {
	t.Parallel()
	m, _ := newRecordManagerConn(t)
	write := func(id string, status vm.InstanceState, desired vm.DesiredState) {
		record := (&vm.VM{ID: id, AccountID: "111122223333", Status: status, DesiredState: desired, LastNode: "node-gone"}).Record()
		require.NoError(t, m.WriteInstanceRecord(id, record))
	}
	write("i-operator-stopped", vm.StateStopped, vm.DesiredStopped)
	write("i-drain-stopped", vm.StateStopped, vm.DesiredRunning)
	write("i-orphaned-running", vm.StateRunning, vm.DesiredRunning)
	write("i-shutting-down", vm.StateShuttingDown, vm.DesiredRunning)
	write("i-terminated", vm.StateTerminated, vm.DesiredRunning)

	live, err := m.ListNonTerminatedInstances()

	require.NoError(t, err)
	var ids []string
	for _, v := range live {
		ids = append(ids, v.ID)
	}
	assert.ElementsMatch(t, []string{"i-operator-stopped", "i-drain-stopped", "i-orphaned-running", "i-shutting-down"}, ids)
}
