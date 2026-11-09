package vm

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS7_FailedRunInstancesDeletesOnlyTheStoreItSeeded enforces ADR-0003:S7
// for the store a RunInstances seeds: when the launch fails the instance, its
// own store goes to deleting and its claim is released, and nothing else.
func TestS7_FailedRunInstancesDeletesOnlyTheStoreItSeeded(t *testing.T) {
	f := newUEFIFixture(t)
	rt := f.withLifecycle(t)
	instance := f.newInstance()
	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), instance))
	f.m.Insert(instance)
	terminated := rt.waitFor(instance.ID, StateTerminated)

	f.m.MarkFailed(t.Context(), instance, "launch_failed")
	select {
	case <-terminated:
	case <-time.After(markFailedDeadline):
		t.Fatalf("ADR-0003:S7: a failed RunInstances did not terminate within %s", markFailedDeadline)
	}
	f.m.WaitForBackgroundWork()

	rec, _ := f.record(t)
	assert.Equal(t, uefistore.StateDeleting, rec.State, "ADR-0003:S7: the store the failed launch seeded goes to the store reaper")
	_, held := f.claimHolder(t)
	assert.False(t, held, "the failed launch releases the claim it took")
}

// TestS7_FailedStartOfAnExistingInstanceKeepsItsStore enforces ADR-0003:S7:
// MarkFailed on an instance that has run is never a RunInstances failure, so
// its store record stays live with its pointer and generation.
func TestS7_FailedStartOfAnExistingInstanceKeepsItsStore(t *testing.T) {
	f := newUEFIFixture(t)
	rt := f.withLifecycle(t)
	key := keyFor("existing")
	instance := f.instance(t, key, f.identity)
	instance.Status = StatePending
	want, _ := f.record(t)
	errored := rt.waitFor(instance.ID, StateError)

	f.m.MarkFailed(t.Context(), instance, "launch_failed")
	select {
	case <-errored:
	case <-time.After(markFailedDeadline):
		t.Fatalf("MarkFailed on an instance that has run did not reach error within %s", markFailedDeadline)
	}
	f.m.WaitForBackgroundWork()

	got, _ := f.record(t)
	assert.Equal(t, want, got, "ADR-0003:S7: an existing instance's store is not touched by a failed start")
}

// TestS7_SeedingNeverWritesOverAnExistingStore enforces ADR-0003:S7 at the
// seed and through the failed RunInstances that follows it: a store record or
// claim that already exists for the ID is refused, and neither the seed nor
// MarkFailed's terminate writes, marks or unlocks it, or puts an object.
func TestS7_SeedingNeverWritesOverAnExistingStore(t *testing.T) {
	failRunInstances := func(t *testing.T, f *uefiFixture, rt *recordedTransitions, instance *VM) {
		t.Helper()
		f.m.Insert(instance)
		terminated := rt.waitFor(instance.ID, StateTerminated)
		f.m.MarkFailed(t.Context(), instance, "volume_preparation_failed")
		select {
		case <-terminated:
		case <-time.After(markFailedDeadline):
			t.Fatalf("a failed RunInstances did not terminate within %s", markFailedDeadline)
		}
		f.m.WaitForBackgroundWork()
	}
	t.Run("record", func(t *testing.T) {
		f := newUEFIFixture(t)
		rt := f.withLifecycle(t)
		_, err := f.records.Create(t.Context(), &uefistore.UEFIStoreRecord{
			InstanceID: testUEFIInstance, AccountID: "111122223333", State: uefistore.StateLive,
			Pointer: &uefistore.UEFIVarStoreRef{Key: keyFor("existing"), Firmware: f.identity, WriterNode: "node-c", WrittenAt: time.Unix(1700000000, 0).UTC()},
		})
		require.NoError(t, err)
		want, wantRev := f.record(t)
		instance := f.newInstance()

		err = f.m.SeedUEFIVarStore(t.Context(), instance)
		require.ErrorIs(t, err, ErrUEFIVarStoreExists)
		failRunInstances(t, f, rt, instance)

		got, gotRev := f.record(t)
		assert.Equal(t, wantRev, gotRev, "ADR-0003:S7: the existing record is not written or marked deleting")
		assert.Equal(t, want, got)
		_, held := f.claimHolder(t)
		assert.False(t, held, "the claim the refused seed took is released")
		assert.Empty(t, f.objectKeys(t))
	})
	t.Run("claim", func(t *testing.T) {
		f := newUEFIFixture(t)
		rt := f.withLifecycle(t)
		peer, err := f.peerClaim("node-b").TryLock(t.Context())
		require.NoError(t, err)
		_, err = f.records.Create(t.Context(), &uefistore.UEFIStoreRecord{
			InstanceID: testUEFIInstance, AccountID: "111122223333", State: uefistore.StateLive, Generation: peer.Generation,
		})
		require.NoError(t, err)
		instance := f.newInstance()

		err = f.m.SeedUEFIVarStore(t.Context(), instance)
		require.ErrorIs(t, err, ErrUEFIVarStoreExists)
		failRunInstances(t, f, rt, instance)

		holder, held := f.claimHolder(t)
		require.True(t, held)
		assert.Equal(t, peer, holder, "ADR-0003:S7: another node's claim is left with it")
		rec, _ := f.record(t)
		assert.Equal(t, uefistore.StateLive, rec.State, "ADR-0003:S7: another node's store is not marked deleting")
		assert.Empty(t, f.objectKeys(t))
	})
}
