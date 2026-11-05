package vm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agedObjectStore lists each object as modified when the test says, so a
// prune's age rule can be exercised without waiting.
type agedObjectStore struct {
	*objectstore.MemoryObjectStore

	modified map[string]time.Time
}

func (s *agedObjectStore) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	out, err := s.MemoryObjectStore.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, err
	}
	for _, obj := range out.Contents {
		if at, ok := s.modified[aws.StringValue(obj.Key)]; ok {
			obj.LastModified = aws.Time(at)
		}
	}
	return out, nil
}

// A prune deletes only objects no pointer names that are over 10 minutes old.
// The object the record points at survives however old it is, and so does a
// young object whose pointer move may not have landed yet (ADR-0003:S7).
func TestS7_PruneNeverDeletesThePointedObjectOrAYoungOne(t *testing.T) {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	f.m.finishUEFIWriteBack(instance)
	pointed := f.mustRecord(t).Pointer.Key
	stale, young := keyFor("stale"), keyFor("young")
	f.putObject(t, stale, gzipped(t, region("stale")))
	f.putObject(t, young, gzipped(t, region("young")))
	now := clock.Now()
	f.m.deps.UEFIVarStore.Objects = &agedObjectStore{MemoryObjectStore: f.objects, modified: map[string]time.Time{
		pointed: now.Add(-24 * time.Hour),
		stale:   now.Add(-uefistore.PruneMinAge - time.Second),
		young:   now.Add(-uefistore.PruneMinAge + time.Second),
	}}
	f.m.uefi.queuePrune(testUEFIInstance)

	_, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{pointed, young}, f.objectKeys(t),
		"ADR-0003:S7: a prune must never delete the object the store record points at, or one younger than 10 minutes")
	assert.Contains(t, f.m.uefi.drainPrune(), testUEFIInstance, "a prefix with a young leftover is pruned again")
}

// A prune of a store being deleted, or one with no pointer, deletes nothing:
// it has no pointer to keep, so it cannot tell what is safe (ADR-0003:S7).
func TestS7_PruneOfAStoreWithNoLivePointerDeletesNothing(t *testing.T) {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	f.m.finishUEFIWriteBack(instance)
	stale := keyFor("stale")
	f.putObject(t, stale, gzipped(t, region("stale")))
	f.m.deps.UEFIVarStore.Objects = &agedObjectStore{MemoryObjectStore: f.objects, modified: map[string]time.Time{
		stale: clock.Now().Add(-24 * time.Hour),
	}}
	_, err := f.records.MarkDeleting(t.Context(), testUEFIInstance)
	require.NoError(t, err)
	f.m.uefi.queuePrune(testUEFIInstance)

	_, err = f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.NoError(t, err)

	assert.Contains(t, f.objectKeys(t), stale, "ADR-0003:S7: only the store reaper deletes a deleting store's objects")
}

// The reaper gives up an owed write-back, and removes the copy holding it,
// only when another node holds the claim. While the claim is this node's the
// copy and the mark are kept however often the write fails (ADR-0003:S7).
func TestS7_ReaperDropsAnOwedWriteBackOnlyWhenTheClaimMoved(t *testing.T) {
	owe := oweUEFIWriteBack
	t.Run("claim still ours", func(t *testing.T) {
		f := owe(t)
		for range 3 {
			_, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
			require.NoError(t, err)
		}
		assert.True(t, f.seed(t).WriteBackOwed, "ADR-0003:S7: an owed write-back is never dropped while this node holds the claim")
		assert.Equal(t, region("owed"), f.localVars(t))
		holder, held := f.claimHolder(t)
		require.True(t, held)
		assert.Equal(t, "node-a", holder.Holder)
	})

	t.Run("guest running here", func(t *testing.T) {
		f := owe(t)
		f.m.deps.UEFIVarStore.Objects = f.objects
		old, _ := f.claimHolder(t)
		_, err := f.peerClaim("node-b").TakeOver(t.Context(), old)
		require.NoError(t, err)
		require.True(t, f.m.UpdateState(testUEFIInstance, func(v *VM) { v.Status = StateRunning }))

		_, err = f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
		require.NoError(t, err)

		assert.True(t, f.seed(t).WriteBackOwed, "ADR-0003:S7: the reaper never touches the store of a guest that may run here")
	})

	t.Run("claim moved", func(t *testing.T) {
		f := owe(t)
		old, _ := f.claimHolder(t)
		_, err := f.peerClaim("node-b").TakeOver(t.Context(), old)
		require.NoError(t, err)
		lost := uefiCounter(t, "mulga.vm.uefi_store.write_backs_lost", "", "")

		n, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
		require.NoError(t, err)

		assert.Equal(t, 1, n)
		_, statErr := os.Stat(f.instanceDir())
		require.ErrorIs(t, statErr, os.ErrNotExist)
		assert.Equal(t, lost+1, uefiCounter(t, "mulga.vm.uefi_store.write_backs_lost", "", ""))
		holder, _ := f.claimHolder(t)
		assert.Equal(t, "node-b", holder.Holder, "the new owner's claim is left alone")
	})
}

// oweUEFIWriteBack stops a running instance whose last write-back fails, so
// seeded.json is marked owed and the claim kept.
func oweUEFIWriteBack(t *testing.T) *uefiFixture {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	f.m.startUEFIWriter(testUEFIInstance, false)
	f.guestWrites(t, "owed")
	clock.waitArmed(t)
	f.m.deps.UEFIVarStore.Objects = failingPutStore{f.objects}
	f.m.finishUEFIWriteBack(instance)
	require.True(t, f.m.UpdateState(testUEFIInstance, func(v *VM) { v.Status = StateStopped }))
	require.True(t, f.seed(t).WriteBackOwed)
	return f
}

// When the store record refuses the owed write-back's pointer move, the reaper
// removes only the local copy: the record, its pointer and every object stay
// for the claim's new holder or the store reaper (ADR-0003:S7).
func TestS7_ReaperRemovesOnlyTheLocalCopyWhenTheRecordRefusesTheMove(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(t *testing.T, f *uefiFixture)
		lost   int64
	}{
		{"fenced by a later claim", func(t *testing.T, f *uefiFixture) {
			old, _ := f.claimHolder(t)
			tok, err := f.peerClaim("node-b").TakeOver(t.Context(), old)
			require.NoError(t, err)
			_, err = f.records.Stamp(t.Context(), testUEFIInstance, tok.Generation)
			require.NoError(t, err)
			require.NoError(t, f.peerClaim("node-b").Unlock(t.Context(), tok))
		}, 1},
		{"record deleting", func(t *testing.T, f *uefiFixture) {
			_, err := f.records.MarkDeleting(t.Context(), testUEFIInstance)
			require.NoError(t, err)
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := oweUEFIWriteBack(t)
			f.m.deps.UEFIVarStore.Objects = f.objects
			tc.refuse(t, f)
			pointer := f.mustRecord(t).Pointer.Key
			lost := uefiCounter(t, "mulga.vm.uefi_store.write_backs_lost", "", "")

			n, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
			require.NoError(t, err)

			assert.Equal(t, 1, n)
			_, statErr := os.Stat(f.instanceDir())
			require.ErrorIs(t, statErr, os.ErrNotExist)
			assert.Equal(t, lost+tc.lost, uefiCounter(t, "mulga.vm.uefi_store.write_backs_lost", "", ""))
			assert.Equal(t, pointer, f.mustRecord(t).Pointer.Key, "ADR-0003:S7: a refused move must leave the record's pointer alone")
			assert.Contains(t, f.objectKeys(t), pointer, "ADR-0003:S7: the reaper must never delete the object the record points at")
		})
	}
}

// A crash, a fence and a superseded instance stop the writer without writing
// back, and never drop an owed write-back: the mark, the copy and the claim
// stay for the restart or the reaper (ADR-0003:S7).
func TestS7_PathsThatSkipTheWriteBackKeepAnOwedOne(t *testing.T) {
	for _, path := range []struct {
		name   string
		unlock bool
	}{{"crash", false}, {"fence", true}, {"superseded", true}} {
		t.Run(path.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			_, clock := f.running(t)
			tok, _ := f.claimHolder(t)
			f.m.startUEFIWriter(testUEFIInstance, false)
			f.guestWrites(t, "owed")
			clock.waitArmed(t)
			skips := uefiCounter(t, "mulga.vm.uefi_store.write_back_skips", "path", path.name)

			f.m.dropUEFIWriter(testUEFIInstance, path.name, path.unlock)

			assert.Equal(t, skips+1, uefiCounter(t, "mulga.vm.uefi_store.write_back_skips", "path", path.name))
			assert.True(t, f.seed(t).WriteBackOwed, "ADR-0003:S7: a path that skips the write-back must not drop an owed one")
			assert.Equal(t, region("owed"), f.localVars(t))
			holder, held := f.claimHolder(t)
			require.True(t, held)
			assert.Equal(t, tok, holder)
		})
	}
}

// A fenced or superseded instance with nothing owed gives its claim up, so the
// node that owns it now can start it.
func TestDropUEFIWriter_InstanceThatMovedWithNothingOwedUnlocks(t *testing.T) {
	f := newUEFIFixture(t)
	f.running(t)
	f.m.startUEFIWriter(testUEFIInstance, false)

	f.m.dropUEFIWriter(testUEFIInstance, "superseded", true)

	_, held := f.claimHolder(t)
	assert.False(t, held)
}

// A stop whose writer never consumed the guest's last write still sees vars.fd
// differ from the pointed object, writes it back and only then unlocks; a
// drop that skips the write-back keeps the claim (ADR-0003:S7).
func TestS7_AnUnseenFinalWriteIsNeverLeftBehindAnUnlockedClaim(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		f := newUEFIFixture(t)
		instance, _ := f.running(t)
		want := f.guestWrites(t, "final")

		f.m.finishUEFIWriteBack(instance)

		assert.Equal(t, want, f.pointedRegion(t), "ADR-0003:S7: the stop must write back a change no event reported")
		_, held := f.claimHolder(t)
		assert.False(t, held)
	})
	t.Run("drop", func(t *testing.T) {
		f := newUEFIFixture(t)
		f.running(t)
		tok, _ := f.claimHolder(t)
		f.guestWrites(t, "final")

		f.m.dropUEFIWriter(testUEFIInstance, "superseded", true)

		holder, held := f.claimHolder(t)
		require.True(t, held, "ADR-0003:S7: a drop must not unlock a claim whose copy differs from the pointer")
		assert.Equal(t, tok, holder)
		assert.Equal(t, region("final"), f.localVars(t))
	})
}

// A claim read showing a generation older than this node's copy is a lagging
// replica, not a new owner: the reaper keeps the owed copy and its mark
// (ADR-0003:S7).
func TestS7_ReaperKeepsAnOwedCopyWhenTheClaimReadIsOlder(t *testing.T) {
	f := oweUEFIWriteBack(t)
	f.m.deps.UEFIVarStore.Objects = f.objects
	old, _ := f.claimHolder(t)
	peer, err := f.peerClaim("node-b").TakeOver(t.Context(), old)
	require.NoError(t, err)
	seed := f.seed(t)
	seed.Generation = peer.Generation + 1
	require.NoError(t, writeUEFISeed(f.instanceDir(), seed))
	lost := uefiCounter(t, "mulga.vm.uefi_store.write_backs_lost", "", "")

	_, err = f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.NoError(t, err)

	assert.True(t, f.seed(t).WriteBackOwed, "ADR-0003:S7: an older claim read must not drop an owed write-back")
	assert.Equal(t, region("owed"), f.localVars(t))
	assert.Equal(t, lost, uefiCounter(t, "mulga.vm.uefi_store.write_backs_lost", "", ""))
}

// staleRecordGet answers Get with a record a lagging replica still holds,
// while the leader read sees the current one.
type staleRecordGet struct {
	UEFIStoreRecords

	stale *uefistore.UEFIStoreRecord
}

func (s staleRecordGet) Get(context.Context, string) (*uefistore.UEFIStoreRecord, uint64, error) {
	rec := *s.stale
	return &rec, 1, nil
}

// A prune protects the pointer the stream leader reports, so a replica still
// naming an older pointer cannot expose the current object (ADR-0003:S7).
func TestS7_PruneKeepsTheLeaderPointerWhenAReplicaLags(t *testing.T) {
	f := newUEFIFixture(t)
	_, clock := f.running(t)
	stale := f.mustRecord(t)
	current := keyFor("current")
	f.putObject(t, current, gzipped(t, region("current")))
	ref := *stale.Pointer
	ref.Key = current
	require.NoError(t, f.records.MovePointer(t.Context(), testUEFIInstance, stale.Generation, ref))
	now := clock.Now()
	f.m.deps.UEFIVarStore.Objects = &agedObjectStore{MemoryObjectStore: f.objects, modified: map[string]time.Time{
		stale.Pointer.Key: now.Add(-24 * time.Hour),
		current:           now.Add(-24 * time.Hour),
	}}
	f.m.deps.UEFIVarStore.Records = staleRecordGet{UEFIStoreRecords: f.records, stale: stale}
	f.m.uefi.queuePrune(testUEFIInstance)

	_, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []string{current}, f.objectKeys(t),
		"ADR-0003:S7: a prune must keep the leader's pointer, whatever a replica reports")
}
