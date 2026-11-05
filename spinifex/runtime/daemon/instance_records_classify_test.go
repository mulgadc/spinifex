package daemon_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassify_MapsEachRecordLocationAndShape(t *testing.T) {
	t.Parallel()
	m := newRecordManager(t)
	sys := &vm.VM{ID: "i-sys", AccountID: awsidentifiers.GlobalAccountID, ManagedBy: tags.ManagedByRDS, Status: vm.StateRunning,
		ENIId: "eni-sys", PublicIPAllocID: "eipalloc-sys"}
	require.NoError(t, m.WriteInstanceRecord(sys.ID, sys.Record()))
	live := &vm.VM{ID: "i-live", AccountID: "111122223333", Status: vm.StateRunning}
	require.NoError(t, m.WriteInstanceRecord(live.ID, live.Record()))
	stopped := &vm.VM{ID: "i-stopped", AccountID: "111122223333"}
	require.NoError(t, m.WriteStoppedInstance(stopped.ID, stopped))
	term := &vm.VM{ID: "i-term", AccountID: "111122223333", Status: vm.StateTerminated}
	require.NoError(t, m.WriteTerminatedInstanceRecord(term.ID, term.Record()))
	termSys := &vm.VM{ID: "i-term-sys", AccountID: awsidentifiers.GlobalAccountID, ManagedBy: tags.ManagedByELBv2, Status: vm.StateTerminated}
	require.NoError(t, m.WriteTerminatedInstanceRecord(termSys.ID, termSys.Record()))
	mgrInCustomer := &vm.VM{ID: "i-mgr-cust", AccountID: "111122223333", ManagedBy: tags.ManagedByRDS, Status: vm.StateRunning}
	require.NoError(t, m.WriteInstanceRecord(mgrInCustomer.ID, mgrInCustomer.Record()))
	sysNoMgr := &vm.VM{ID: "i-sys-nomgr", AccountID: awsidentifiers.GlobalAccountID, Status: vm.StateRunning}
	require.NoError(t, m.WriteInstanceRecord(sysNoMgr.ID, sysNoMgr.Record()))

	for id, want := range map[string]systeminstance.Ownership{
		"i-sys":        systeminstance.OwnershipSystem,
		"i-live":       systeminstance.OwnershipCustomer,
		"i-stopped":    systeminstance.OwnershipCustomer,
		"i-term":       systeminstance.OwnershipCustomer,
		"i-term-sys":   systeminstance.OwnershipSystem,
		"i-mgr-cust":   systeminstance.OwnershipCustomer,
		"i-sys-nomgr":  systeminstance.OwnershipCustomer,
		"i-never-made": systeminstance.OwnershipAbsent,
	} {
		got, err := m.Classify(id)
		require.NoError(t, err, id)
		assert.Equal(t, want, got.Ownership, id)
		if id == "i-sys" {
			assert.Equal(t, "eipalloc-sys", got.PublicIPAllocID)
			assert.Equal(t, "eni-sys", got.ENIID)
		}
	}
}

func TestClassify_ReturnsReadFailuresInsteadOfMappingThem(t *testing.T) {
	t.Parallel()
	for name, m := range map[string]*daemon.JetStreamManager{
		"buckets not initialised": {},
		"nil manager":             nil,
	} {
		got, err := m.Classify("i-any")
		require.Error(t, err, name)
		assert.Empty(t, got.Ownership, name)
	}
}
