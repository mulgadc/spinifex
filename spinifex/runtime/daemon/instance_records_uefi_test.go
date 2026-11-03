package daemon_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func uefiRef(key string) *vm.UEFIVarStoreRef {
	return &vm.UEFIVarStoreRef{
		Key:        uefistore.KeyPrefix("i-1") + key,
		Firmware:   uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: 540672, CodeSHA256: "abc"},
		WriterNode: "node-1",
		WrittenAt:  time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
	}
}

// The owner read names the fields a write-back decides on, and an absent
// record is reported as absent rather than as an error.
func TestLoadUEFIVarStoreOwner_ReadsStatusOwnerAndPointer(t *testing.T) {
	m := newRecordManager(t)

	_, found, err := m.LoadUEFIVarStoreOwner("i-1")
	require.NoError(t, err)
	assert.False(t, found)

	rec := testRecord("i-1")
	rec.Status.UEFIVarStore = uefiRef("a")
	require.NoError(t, m.WriteInstanceRecord("i-1", rec))

	owner, found, err := m.LoadUEFIVarStoreOwner("i-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, vm.StateStopped, owner.Status)
	assert.Equal(t, "node-1", owner.LastNode)
	assert.Equal(t, uefiRef("a"), owner.Pointer)
	assert.NotZero(t, owner.Revision)
}

// The pointer moves only at the revision the decision was made against, and
// the move changes nothing else on the record.
func TestCompareAndSetUEFIVarStore_MovesOnlyThePointerAtTheRevisionRead(t *testing.T) {
	m := newRecordManager(t)
	rec := testRecord("i-1")
	rec.Status.UEFIVarStore = uefiRef("a")
	require.NoError(t, m.WriteInstanceRecord("i-1", rec))
	owner, _, err := m.LoadUEFIVarStoreOwner("i-1")
	require.NoError(t, err)

	require.NoError(t, m.CompareAndSetUEFIVarStore("i-1", uefiRef("b"), owner.Revision))

	got, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	want := testRecord("i-1")
	want.Status.UEFIVarStore = uefiRef("b")
	assert.Equal(t, want, got)

	err = m.CompareAndSetUEFIVarStore("i-1", uefiRef("c"), owner.Revision)
	require.ErrorIs(t, err, kvstore.ErrConflict, "a record written since the read must refuse the move")
	got, err = m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, uefiRef("b"), got.Status.UEFIVarStore)

	require.ErrorIs(t, m.CompareAndSetUEFIVarStore("i-2", uefiRef("c"), 1), kvstore.ErrNotFound)
}
