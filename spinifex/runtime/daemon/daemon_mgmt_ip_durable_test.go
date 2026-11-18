package daemon

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon/mgmtipstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var durableTestPrefix = netip.MustParsePrefix("10.15.8.0/24")

// createDurableMgmtBucket provisions the durable bucket as the cutover would and
// seeds the subnet's record with entries.
func createDurableMgmtBucket(t *testing.T, jsm *JetStreamManager, entries ...mgmtipstore.Entry) *mgmtipstore.Store {
	t.Helper()
	store, err := mgmtipstore.Create(t.Context(), jsm.js, MgmtIPAMBucket, "test")
	require.NoError(t, err)
	_, err = store.Seed(t.Context(), durableTestPrefix, mgmtipstore.AuthorityMigrated, entries)
	require.NoError(t, err)
	return store
}

func newDurableAllocator(t *testing.T, jsm *JetStreamManager, node string) *MgmtIPAllocator {
	t.Helper()
	a, err := NewMgmtIPAllocator("10.15.8.1")
	require.NoError(t, err)
	require.NoError(t, a.SetMode(MgmtIPAMDurable))
	a.BindKV(jsm, node)
	return a
}

func durableEntries(t *testing.T, store *mgmtipstore.Store) []mgmtipstore.Entry {
	t.Helper()
	snap, err := store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)
	return snap.Entries()
}

func durableEntry(ip, id, node string) mgmtipstore.Entry {
	return mgmtipstore.Entry{IP: ip, InstanceID: id, Node: node}
}

// requireNoLegacyRecord proves durable mode never wrote the expiring record.
func requireNoLegacyRecord(t *testing.T, jsm *JetStreamManager) {
	t.Helper()
	_, err := jsm.clusterKV.Get(t.Context(), mgmtIPAMKeyPrefix+durableTestPrefix.String())
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

func TestMgmtIPAllocator_SetModeRefusesUnknownMode(t *testing.T) {
	a, err := NewMgmtIPAllocator("10.15.8.1")
	require.NoError(t, err)
	require.Error(t, a.SetMode("durabel"))
	assert.False(t, a.isDurable())
}

func TestMgmtIPAllocator_Durable_AllocatesFromTheDurableRecordOnly(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-existing", "node2"))
	a := newDurableAllocator(t, jsm, "node1")

	ip, err := a.Allocate("i-new")
	require.NoError(t, err)
	assert.Equal(t, "10.15.8.11", ip)
	assert.Contains(t, durableEntries(t, store), durableEntry("10.15.8.11", "i-new", "node1"))
	requireNoLegacyRecord(t, jsm)
}

func TestMgmtIPAllocator_Durable_TwoNodesNeverShareAnAddress(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm)
	a := newDurableAllocator(t, jsm, "node1")
	b := newDurableAllocator(t, jsm, "node2")

	type result struct {
		ip  string
		err error
	}
	results := make(chan result, 20)
	for i := range 10 {
		go func() {
			ip, err := a.Allocate(fmtID("a", i))
			results <- result{ip, err}
		}()
		go func() {
			ip, err := b.Allocate(fmtID("b", i))
			results <- result{ip, err}
		}()
	}
	seen := map[string]bool{}
	for range 20 {
		r := <-results
		require.NoError(t, r.err)
		require.False(t, seen[r.ip], "address %s handed out twice", r.ip)
		seen[r.ip] = true
	}
	assert.Len(t, durableEntries(t, store), 20)
}

func fmtID(prefix string, i int) string { return fmt.Sprintf("%s-%d", prefix, i) }

func TestMgmtIPAllocator_Durable_RefusesWithoutFallingBack(t *testing.T) {
	t.Run("bucket absent", func(t *testing.T) {
		jsm := newTestMgmtJSM(t)
		a := newDurableAllocator(t, jsm, "node1")
		_, err := a.Allocate("i-1")
		require.Error(t, err)
		requireNoLegacyRecord(t, jsm)
		_, err = jsm.js.KeyValue(t.Context(), MgmtIPAMBucket)
		require.ErrorIs(t, err, jetstream.ErrBucketNotFound)
	})
	t.Run("record not seeded", func(t *testing.T) {
		jsm := newTestMgmtJSM(t)
		_, err := mgmtipstore.Create(t.Context(), jsm.js, MgmtIPAMBucket, "test")
		require.NoError(t, err)
		a := newDurableAllocator(t, jsm, "node1")
		_, err = a.Allocate("i-1")
		require.ErrorIs(t, err, mgmtipstore.ErrNotAuthoritative)
		requireNoLegacyRecord(t, jsm)
	})
	t.Run("not yet bound, even for a cached instance", func(t *testing.T) {
		a, err := NewMgmtIPAllocator("10.15.8.1")
		require.NoError(t, err)
		require.NoError(t, a.SetMode(MgmtIPAMDurable))
		a.Rebuild(map[string]*vm.VM{"i-1": {MgmtIP: "10.15.8.20"}})
		_, err = a.Allocate("i-1")
		require.ErrorIs(t, err, errMgmtIPAMUnbound)
	})
}

func TestMgmtIPAllocator_Durable_CachedAddressIsReclaimedNotTrusted(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm)
	a := newDurableAllocator(t, jsm, "node1")
	ip, err := a.Allocate("i-1")
	require.NoError(t, err)

	again, err := a.Allocate("i-1")
	require.NoError(t, err)
	assert.Equal(t, ip, again)

	// The record lost i-1 and gave its address to i-2 elsewhere.
	snap, err := store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)
	_, err = store.Remove(t.Context(), snap, "i-1", "node1", ip)
	require.NoError(t, err)
	snap, err = store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)
	_, err = store.ClaimAt(t.Context(), snap, "i-2", "node2", ip)
	require.NoError(t, err)

	_, err = a.Allocate("i-1")
	require.ErrorIs(t, err, errMgmtIPNotReserved)
	assert.Equal(t, []mgmtipstore.Entry{durableEntry(ip, "i-2", "node2")}, durableEntries(t, store))
}

// A cached holder the record has lost may have had its address handed out while
// it was missing, so it is refused rather than written back.
func TestMgmtIPAllocator_Durable_CachedHolderMissingFromTheRecordIsRefused(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm)
	a := newDurableAllocator(t, jsm, "node1")
	a.Rebuild(map[string]*vm.VM{"i-1": {MgmtIP: "10.15.8.10"}})
	before, err := store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)

	_, err = a.Allocate("i-1")
	require.ErrorIs(t, err, errMgmtIPNotReserved)
	after, err := store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)
	assert.Equal(t, before.Revision(), after.Revision())
	assert.Empty(t, after.Entries())
}

func TestMgmtIPAllocator_Durable_BindsOnceTheBucketAppears(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	a := newDurableAllocator(t, jsm, "node1")
	_, err := a.Allocate("i-1")
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound)

	createDurableMgmtBucket(t, jsm)
	ip, err := a.Allocate("i-1")
	require.NoError(t, err)
	assert.Equal(t, "10.15.8.10", ip)
}

func TestMgmtIPAllocator_Durable_RefusesAnInstanceHeldOnAnotherNode(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-1", "node2"))
	a := newDurableAllocator(t, jsm, "node1")

	_, err := a.Allocate("i-1")
	require.ErrorIs(t, err, mgmtipstore.ErrForeignHolder)
	a.Claim("i-1", "10.15.8.10")
	_, err = a.Allocate("i-1")
	require.ErrorIs(t, err, mgmtipstore.ErrForeignHolder)
}

func TestMgmtIPAllocator_Durable_ReleaseFreesOnlyTheKnownAddress(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.30", "i-unknown", "node1"))
	a := newDurableAllocator(t, jsm, "node1")
	ip, err := a.Allocate("i-1")
	require.NoError(t, err)

	a.Release("i-1")
	a.Release("i-unknown")
	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.30", "i-unknown", "node1")}, durableEntries(t, store))
	assert.NotContains(t, a.allocated, "i-1", "released address %s still cached", ip)
}

func TestMgmtIPAllocator_Durable_ReleaseOfAnotherNodesEntryKeepsIt(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-1", "node2"))
	a := newDurableAllocator(t, jsm, "node1")
	a.allocated["i-1"] = "10.15.8.10"

	a.Release("i-1")
	require.Error(t, a.ReleaseChecked("i-1"))
	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", "i-1", "node2")}, durableEntries(t, store))
}

// An allocation whose outcome was unknown may have landed although nothing was
// cached; ReleaseChecked must still remove it.
func TestMgmtIPAllocator_Durable_ReleaseCheckedRemovesALandedUnknownAllocation(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.12", "lambda-a", "node1"))
	a := newDurableAllocator(t, jsm, "node1")

	require.NoError(t, a.ReleaseChecked("lambda-a"))
	assert.Empty(t, durableEntries(t, store))
}

func TestMgmtIPAllocator_Durable_ReleaseCheckedFencesAnAbsentAllocation(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm)
	delayed, err := store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)
	a := newDurableAllocator(t, jsm, "node1")

	require.NoError(t, a.ReleaseChecked("lambda-a"))
	_, err = store.Insert(t.Context(), delayed, "lambda-a", "node1")
	require.ErrorIs(t, err, mgmtipstore.ErrConflict)
	assert.Empty(t, durableEntries(t, store))
}

func TestMgmtIPAllocator_Durable_ReleaseCheckedUnboundFails(t *testing.T) {
	a, err := NewMgmtIPAllocator("10.15.8.1")
	require.NoError(t, err)
	require.NoError(t, a.SetMode(MgmtIPAMDurable))
	require.ErrorIs(t, a.ReleaseChecked("lambda-a"), errMgmtIPAMUnbound)
}

func TestMgmtIPAllocator_Durable_ClaimRefusesAnotherInstancesAddress(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-other", "node2"))
	a := newDurableAllocator(t, jsm, "node1")

	a.Claim("i-1", "10.15.8.10")
	a.Claim("i-1", "10.15.8.40")
	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", "i-other", "node2")}, durableEntries(t, store))
}

func TestMgmtIPAllocator_Durable_ClaimReacknowledgesOwnReservation(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-1", "node1"))
	a := newDurableAllocator(t, jsm, "node1")
	before, err := store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)

	a.Claim("i-1", "10.15.8.10")
	after, err := store.Load(t.Context(), durableTestPrefix)
	require.NoError(t, err)
	assert.Greater(t, after.Revision(), before.Revision())
	assert.Equal(t, before.Entries(), after.Entries())
}

func TestMgmtIPAllocator_Durable_RebuildNeverReconstructsOrOverwrites(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	want := []mgmtipstore.Entry{
		durableEntry("10.15.8.10", "i-elsewhere", "node2"),
		durableEntry("10.15.8.11", "i-moved", "node2"),
		durableEntry("10.15.8.12", "i-local", "node1"),
	}
	store := createDurableMgmtBucket(t, jsm, want...)
	a := newDurableAllocator(t, jsm, "node1")

	a.Rebuild(map[string]*vm.VM{
		"i-local":    {MgmtIP: "10.15.8.12"},
		"i-missing":  {MgmtIP: "10.15.8.20"},
		"i-conflict": {MgmtIP: "10.15.8.10"},
		"i-moved":    {MgmtIP: "10.15.8.11"},
		"i-none":     {},
	})
	assert.Equal(t, want, durableEntries(t, store))
	requireNoLegacyRecord(t, jsm)
}

// A running guest whose address the record lacks must not have it handed to
// another instance: new allocations wait until the record acknowledges it.
func TestMgmtIPAllocator_Durable_UnreservedLiveGuestBlocksNewAllocations(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm)
	a := newDurableAllocator(t, jsm, "node1")
	a.Rebuild(map[string]*vm.VM{"i-1": {MgmtIP: "10.15.8.10"}})

	_, err := a.Allocate("i-2")
	require.ErrorIs(t, err, errMgmtIPUnresolvedHolder)
	assert.Empty(t, durableEntries(t, store))

	// Once the cutover records the guest, allocation resumes around it.
	_, err = store.Seed(t.Context(), durableTestPrefix, mgmtipstore.AuthorityMigrated,
		[]mgmtipstore.Entry{durableEntry("10.15.8.10", "i-1", "node1")})
	require.NoError(t, err)
	ip, err := a.Allocate("i-2")
	require.NoError(t, err)
	assert.Equal(t, "10.15.8.11", ip)
}

func TestMgmtIPAllocator_Durable_ConflictingLiveGuestBlocksNewAllocations(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-other", "node2"))
	a := newDurableAllocator(t, jsm, "node1")
	a.Rebuild(map[string]*vm.VM{"i-1": {MgmtIP: "10.15.8.10"}})

	_, err := a.Allocate("i-2")
	require.ErrorIs(t, err, errMgmtIPUnresolvedHolder)
	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", "i-other", "node2")}, durableEntries(t, store))
}

func TestMgmtIPAllocator_Durable_UnboundAtRebuildBlocksUntilConfirmed(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	a := newDurableAllocator(t, jsm, "node1")
	a.Rebuild(map[string]*vm.VM{"i-1": {MgmtIP: "10.15.8.10"}})

	createDurableMgmtBucket(t, jsm)
	_, err := a.Allocate("i-2")
	require.ErrorIs(t, err, errMgmtIPUnresolvedHolder)
}

// Confirmation belongs to an address, so a local guest reported later at a
// different address is unconfirmed again and blocks new allocations.
func TestMgmtIPAllocator_Durable_ChangedLocalAddressIsUnconfirmedAgain(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-1", "node1"))
	a := newDurableAllocator(t, jsm, "node1")
	a.Rebuild(map[string]*vm.VM{"i-1": {MgmtIP: "10.15.8.10"}})
	a.Rebuild(map[string]*vm.VM{"i-1": {MgmtIP: "10.15.8.11"}})

	_, err := a.Allocate("i-2")
	require.ErrorIs(t, err, errMgmtIPUnresolvedHolder)
	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", "i-1", "node1")}, durableEntries(t, store))
}
