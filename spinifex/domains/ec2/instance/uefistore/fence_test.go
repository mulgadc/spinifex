package uefistore_test

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func revisionOf(t *testing.T, s *uefistore.Store) uint64 {
	t.Helper()
	_, rev, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	return rev
}

func TestStamp_RaisesTheGenerationAndReturnsThePointerToBootFrom(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), pointedRecord())
	require.NoError(t, err)

	rec, err := s.Stamp(t.Context(), instanceID, 9)
	require.NoError(t, err)
	assert.Equal(t, uint64(9), rec.Generation)
	assert.Equal(t, pointer("b"), rec.Pointer)

	got, _, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, rec, got, "the returned record is the one stored")
}

// A daemon restart adopts its claim at the same generation. The stamp still
// writes by CAS, so a pointer read from a lagging follower fails the write and
// is read again rather than booted from.
func TestStamp_AtTheRecordsGenerationConfirmsTheReadByCAS(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), pointedRecord())
	require.NoError(t, err)
	before := revisionOf(t, s)

	rec, err := s.Stamp(t.Context(), instanceID, 7)
	require.NoError(t, err)
	assert.Equal(t, pointer("b"), rec.Pointer)
	assert.Equal(t, uint64(7), rec.Generation)
	assert.Greater(t, revisionOf(t, s), before, "the read must be confirmed by a revision-checked write")
}

func TestStamp_RefusesAndChangesNothing(t *testing.T) {
	tests := []struct {
		name       string
		deleting   bool
		generation uint64
		want       error
	}{
		{name: "a later acquisition stamped it", generation: 6, want: uefistore.ErrFenced},
		{name: "the store is being deleted", deleting: true, generation: 8, want: uefistore.ErrDeleting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openStore(t)
			rev, err := s.Create(t.Context(), pointedRecord())
			require.NoError(t, err)
			if tt.deleting {
				_, err = s.MarkDeleting(t.Context(), instanceID)
				require.NoError(t, err)
				rev = revisionOf(t, s)
			}

			_, err = s.Stamp(t.Context(), instanceID, tt.generation)
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, rev, revisionOf(t, s))
		})
	}
}

func TestStamp_MissingRecordIsNotFound(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Stamp(t.Context(), instanceID, 1)
	require.ErrorIs(t, err, kvstore.ErrNotFound)
}

// The record keeps only the current version, so each move replaces the
// pointer and names nothing it replaced.
func TestMovePointer_ReplacesThePointer(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), pointedRecord())
	require.NoError(t, err)

	for _, name := range []string{"c", "d"} {
		require.NoError(t, s.MovePointer(t.Context(), instanceID, 7, *pointer(name)))
	}

	got, _, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, pointer("d"), got.Pointer)
	assert.Equal(t, uint64(7), got.Generation)
}

func TestMovePointer_ToTheCurrentKeyWritesNothing(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.Create(t.Context(), pointedRecord())
	require.NoError(t, err)
	before := revisionOf(t, s)

	require.NoError(t, s.MovePointer(t.Context(), instanceID, 7, *pointer("b")))
	assert.Equal(t, before, revisionOf(t, s))
}

func TestMovePointer_RefusesAndChangesNothing(t *testing.T) {
	tests := []struct {
		name       string
		deleting   bool
		generation uint64
		ref        *uefistore.UEFIVarStoreRef
		want       error
	}{
		{name: "an earlier generation", generation: 6, ref: pointer("c"), want: uefistore.ErrFenced},
		{name: "a later generation than the stamp", generation: 8, ref: pointer("c"), want: uefistore.ErrFenced},
		{name: "a deleting store", deleting: true, generation: 7, ref: pointer("c"), want: uefistore.ErrDeleting},
		{name: "a key under another instance", generation: 7,
			ref:  &uefistore.UEFIVarStoreRef{Key: uefistore.KeyPrefix("i-0fedcba9876543210") + "c", Firmware: pointer("c").Firmware, WriterNode: "n"},
			want: uefistore.ErrInvalidRecord},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openStore(t)
			_, err := s.Create(t.Context(), pointedRecord())
			require.NoError(t, err)
			if tt.deleting {
				_, err = s.MarkDeleting(t.Context(), instanceID)
				require.NoError(t, err)
			}
			rev := revisionOf(t, s)

			err = s.MovePointer(t.Context(), instanceID, tt.generation, *tt.ref)
			require.ErrorIs(t, err, tt.want)
			got, gotRev, err := s.Get(t.Context(), instanceID)
			require.NoError(t, err)
			assert.Equal(t, rev, gotRev)
			assert.Equal(t, pointer("b"), got.Pointer)
		})
	}
}

// claimedStore is a store record and a claim group per node over one
// JetStream, as two daemons see them.
type claimedStore struct {
	records *uefistore.Store
	a, b    *kvsync.Claim
}

func newClaimedStore(t *testing.T) *claimedStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	records := uefistore.NewStore(js)
	require.NoError(t, records.Open(t.Context()))
	_, err := records.Create(t.Context(), liveRecord())
	require.NoError(t, err)
	return &claimedStore{
		records: records,
		a:       uefistore.NewClaims(kvstore.OpenSyncBucket(js), "node-a").Get(instanceID),
		b:       uefistore.NewClaims(kvstore.OpenSyncBucket(js), "node-b").Get(instanceID),
	}
}

// A node partitioned long enough to be taken over is fenced by the new
// owner's stamp: its pointer move fails and the record keeps what it had.
func TestFence_TheOldHoldersMoveFailsAfterTheNewOwnersStamp(t *testing.T) {
	c := newClaimedStore(t)
	first, err := c.a.TryLock(t.Context())
	require.NoError(t, err)
	_, err = c.records.Stamp(t.Context(), instanceID, first.Generation)
	require.NoError(t, err)
	require.NoError(t, c.records.MovePointer(t.Context(), instanceID, first.Generation, *pointer("b")))

	second, err := c.b.TakeOver(t.Context(), first)
	require.NoError(t, err)
	_, err = c.records.Stamp(t.Context(), instanceID, second.Generation)
	require.NoError(t, err)
	rev := revisionOf(t, c.records)

	err = c.records.MovePointer(t.Context(), instanceID, first.Generation, *pointer("late"))
	require.ErrorIs(t, err, uefistore.ErrFenced)
	got, gotRev, err := c.records.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, pointer("b"), got.Pointer)
	assert.Equal(t, second.Generation, got.Generation)
}

// The new owner reads the pointer after it stamps, so a move the old holder
// landed between the takeover and the stamp is the one it boots from.
func TestFence_AMoveBetweenTakeoverAndStampIsThePointerTheNewOwnerBoots(t *testing.T) {
	c := newClaimedStore(t)
	first, err := c.a.TryLock(t.Context())
	require.NoError(t, err)
	_, err = c.records.Stamp(t.Context(), instanceID, first.Generation)
	require.NoError(t, err)

	second, err := c.b.TakeOver(t.Context(), first)
	require.NoError(t, err)
	require.NoError(t, c.records.MovePointer(t.Context(), instanceID, first.Generation, *pointer("landed")))

	rec, err := c.records.Stamp(t.Context(), instanceID, second.Generation)
	require.NoError(t, err)
	assert.Equal(t, keyOf("landed"), rec.Pointer.Key)
}
