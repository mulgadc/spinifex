package daemon_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A resume rewrites the record at the revision naming this node, so a recovery
// claim that read the record before the rewrite loses.
func TestReassertInstanceOwner_BeatsAClaimThatReadTheEarlierRevision(t *testing.T) {
	t.Parallel()
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-1")))

	require.NoError(t, m.ReassertInstanceOwner("i-1", "node-1"))
	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, "node-1", stored.Status.LastNode, "the rewrite leaves the owner unchanged")
}

// A record a claim already moved is not this node's to resume under.
func TestReassertInstanceOwner_RefusesARecordAClaimMoved(t *testing.T) {
	t.Parallel()
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-1")))
	_, err := m.ClaimRecoverableInstance("i-1", "node-1", "node-2")
	require.NoError(t, err)

	assert.ErrorIs(t, m.ReassertInstanceOwner("i-1", "node-1"), vm.ErrRecoveryClaimLost,
		"a guest whose instance a survivor claimed must not resume")
}

// After a resume, a record a claim moved in the meantime is returned to the
// node running the guest, and an unmoved record is left alone.
func TestRepairInstanceOwner_ReturnsAMovedRecord(t *testing.T) {
	t.Parallel()
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", recoverableRecord("i-1", "node-1")))

	require.NoError(t, m.RepairInstanceOwner("i-1", "node-1"), "an unmoved record needs no repair")
	_, err := m.ClaimRecoverableInstance("i-1", "node-1", "node-2")
	require.NoError(t, err)

	require.NoError(t, m.RepairInstanceOwner("i-1", "node-1"))
	stored, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, "node-1", stored.Status.LastNode, "the node holding the leases is the instance's owner")
}
