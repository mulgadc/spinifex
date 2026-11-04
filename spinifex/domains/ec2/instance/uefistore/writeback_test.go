package uefistore_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func markDeleting(t *testing.T, s *uefistore.Store, id string) {
	t.Helper()
	_, err := s.MarkDeleting(t.Context(), id)
	require.NoError(t, err)
}

// An obligation is recorded on the live record, replacing any other, and
// read back as recorded.
func TestRecordWriteBack_SetsTheOneOpenObligation(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)

	got, err := s.LiveWriteBack(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Nil(t, got)

	first := *writeBack()
	require.NoError(t, s.RecordWriteBack(t.Context(), instanceID, first))
	second := first
	second.Node, second.Key = "node-3", uefistore.KeyPrefix(instanceID)+"c"
	require.NoError(t, s.RecordWriteBack(t.Context(), instanceID, second))

	got, err = s.LiveWriteBack(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, &second, got)
}

// A clear for one key leaves an obligation recorded for another, so a retry
// cannot drop an obligation recorded after it read the record.
func TestClearWriteBack_ClearsOnlyTheNamedKey(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)
	wb := *writeBack()
	require.NoError(t, s.RecordWriteBack(t.Context(), instanceID, wb))

	require.NoError(t, s.ClearWriteBack(t.Context(), instanceID, uefistore.KeyPrefix(instanceID)+"other"))
	got, err := s.LiveWriteBack(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, &wb, got)

	require.NoError(t, s.ClearWriteBack(t.Context(), instanceID, wb.Key))
	got, err = s.LiveWriteBack(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Nil(t, got)

	require.NoError(t, s.RecordWriteBack(t.Context(), instanceID, wb))
	require.NoError(t, s.ClearWriteBack(t.Context(), instanceID, ""), "an empty key clears whatever is owed")
	got, err = s.LiveWriteBack(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// A store whose record is missing or deleting owes no write-back, and none
// can be recorded on it.
func TestWriteBack_RecordThatIsNotLiveIsErrNotLive(t *testing.T) {
	s, _ := openStore(t)

	_, err := s.LiveWriteBack(t.Context(), instanceID)
	require.ErrorIs(t, err, uefistore.ErrNotLive)
	require.ErrorIs(t, s.RecordWriteBack(t.Context(), instanceID, *writeBack()), uefistore.ErrNotLive)

	_, err = s.Create(t.Context(), liveRecord())
	require.NoError(t, err)
	markDeleting(t, s, instanceID)

	_, err = s.LiveWriteBack(t.Context(), instanceID)
	require.ErrorIs(t, err, uefistore.ErrNotLive)
	require.ErrorIs(t, s.RecordWriteBack(t.Context(), instanceID, *writeBack()), uefistore.ErrNotLive)
	require.ErrorIs(t, s.ClearWriteBack(t.Context(), instanceID, ""), uefistore.ErrNotLive)
	rec, _, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, uefistore.StateDeleting, rec.State)
	assert.Nil(t, rec.WriteBack, "a deleting record is never given an obligation")
}

// The reaper is handed only the live records whose obligation names its node.
func TestWriteBacksOwedBy_ListsOnlyThisNodesLiveObligations(t *testing.T) {
	s, _ := openStore(t)
	ids := []string{"i-0000000000000000a", "i-0000000000000000b", "i-0000000000000000c", "i-0000000000000000d"}
	for _, id := range ids {
		_, err := s.Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: id, State: uefistore.StateLive})
		require.NoError(t, err)
	}
	owe := func(id, node string) {
		wb := *writeBack()
		wb.Node, wb.Key, wb.SeededFrom = node, uefistore.KeyPrefix(id)+"b", uefistore.KeyPrefix(id)+"a"
		require.NoError(t, s.RecordWriteBack(t.Context(), id, wb))
	}
	owe(ids[0], "node-1")
	owe(ids[1], "node-2")
	owe(ids[2], "node-1")
	markDeleting(t, s, ids[2])

	got, err := s.WriteBacksOwedBy(t.Context(), "node-1")
	require.NoError(t, err)
	assert.Equal(t, []string{ids[0]}, got)
}
