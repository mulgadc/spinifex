package daemon_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pins today: a record that is neither terminated nor operator-stopped is in
// the shared instance KV but in no listing the KV serves. Only a node holding
// the instance in memory can report it, so when none does it is absent from
// the account listing with no error.
func TestCurrentBehaviour_UnheldNonTerminatedRecordIsInNoKVListing(t *testing.T) {
	cases := []struct {
		name    string
		status  vm.InstanceState
		desired vm.DesiredState
	}{
		{name: "drain-stopped, desired running", status: vm.StateStopped, desired: vm.DesiredRunning},
		{name: "orphaned last node, observed running", status: vm.StateRunning, desired: vm.DesiredRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newRecordManagerConn(t)
			record := (&vm.VM{ID: "i-1", AccountID: "111122223333", Status: tc.status, DesiredState: tc.desired, LastNode: "node-gone"}).Record()
			require.NoError(t, m.WriteInstanceRecord("i-1", record))

			records, err := m.ListInstanceRecords()
			require.NoError(t, err)
			require.Len(t, records, 1, "the record is in the shared KV")

			stopped, err := m.ListStoppedInstances()
			require.NoError(t, err)
			assert.Empty(t, stopped, "CURRENT: not in the stopped listing, no error")

			terminated, err := m.ListTerminatedInstances()
			require.NoError(t, err)
			assert.Empty(t, terminated, "CURRENT: not in the terminated listing, no error")
		})
	}
}
