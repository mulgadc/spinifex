package floating

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// fixedFake is a provider whose addresses exist before the pool does: it cannot create, and
// detach and delete change nothing.
type fixedFake struct {
	mu    sync.Mutex
	addrs map[string]*Address
	calls []string
	// onList runs once when the holder pass lists, which is after it read the bindings and before
	// it acts on them.
	onList func()
}

var (
	_ Provider = (*fixedFake)(nil)
	_ FixedSet = (*fixedFake)(nil)
)

func newFixedFake(n int) *fixedFake {
	f := &fixedFake{addrs: map[string]*Address{}}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("eip-%d", i)
		f.addrs[id] = &Address{ID: id, IP: netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", i))}
	}
	return f
}

func (f *fixedFake) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fixedFake) callsWith(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func (f *fixedFake) holder(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrs[id].HolderID
}

func (f *fixedFake) snapshot() []Address {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Address, 0, len(f.addrs))
	for _, a := range f.addrs {
		out = append(out, *a)
	}
	return out
}

func (f *fixedFake) Create(context.Context, string) (Address, error) {
	f.record("create")
	return Address{}, errors.New("a fixed set creates nothing")
}

func (f *fixedFake) Attach(_ context.Context, id, host string) error {
	f.record("attach:" + id + ">" + host)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs[id].HolderID = host
	return nil
}

func (f *fixedFake) Detach(context.Context, string) error { f.record("detach"); return nil }
func (f *fixedFake) Delete(context.Context, string) error { f.record("delete"); return nil }

func (f *fixedFake) Get(_ context.Context, id string) (Address, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.addrs[id]
	if !ok {
		return Address{}, ErrNotFound
	}
	return *a, nil
}

func (f *fixedFake) List(context.Context) ([]Address, error) {
	f.mu.Lock()
	hook := f.onList
	f.onList = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return f.snapshot(), nil
}

func (f *fixedFake) Members(context.Context) ([]Address, error) { return f.snapshot(), nil }

func newFixedNode(t *testing.T, fake *fixedFake, store Store, hostID string) *node {
	t.Helper()
	n := &node{plugged: map[string]struct{}{}, gateway: map[string]bool{}}
	alloc, err := New(fake, store, Config{
		Pool:   external.ExternalPoolConfig{Name: pool, Source: "latitude"},
		HostID: hostID,
		LocalPorts: func(context.Context) (map[string]struct{}, error) {
			return maps.Clone(n.plugged), nil
		},
		LocalGateway: func(_ context.Context, vpcID string) (bool, error) { return n.gateway[vpcID], nil },
	})
	require.NoError(t, err)
	n.alloc = alloc
	return n
}

func fixedSetup(t *testing.T, members int) (*fixedFake, *memStore, *node, *node) {
	t.Helper()
	fake := newFixedFake(members)
	store := newMemStore()
	return fake, store, newFixedNode(t, fake, store, hostA), newFixedNode(t, fake, store, hostB)
}

func TestFixedSet_AllocatePicksAFreeMemberAndCreatesNothing(t *testing.T) {
	fake, store, a, _ := fixedSetup(t, 2)
	ip1 := allocENI(t, a, "eni-1")
	ip2 := allocENI(t, a, "eni-2")

	assert.NotEqual(t, ip1, ip2)
	assert.Equal(t, 0, fake.callsWith("create"))
	b := store.binding(t, ip1)
	assert.NotEmpty(t, b.Gen)
	assert.NotEqual(t, b.Gen, store.binding(t, ip2).Gen)
	assert.Empty(t, fake.holder(b.AddrID), "the owner's node attaches, never the allocating node")
}

func TestFixedSet_NoFreeMemberIsQuotaForTheCall(t *testing.T) {
	_, _, a, _ := fixedSetup(t, 1)
	allocENI(t, a, "eni-1")

	_, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP})
	assert.ErrorContains(t, err, awserrors.ErrorAddressLimitExceeded)
	_, err = a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: "eni-public"})
	assert.ErrorContains(t, err, awserrors.ErrorInsufficientAddressCapacity)
}

func TestFixedSet_ConcurrentAllocationsNeverShareAMember(t *testing.T) {
	fake := newFixedFake(3)
	store := newMemStore()
	const callers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[netip.Addr]int{}
	failed := 0
	for i := range callers {
		n := newFixedNode(t, fake, store, hostA)
		wg.Go(func() {
			ip, err := n.alloc.Allocate(context.Background(), external.AllocateRequest{
				PoolName: pool, Purpose: "eni-public", ENIID: fmt.Sprintf("eni-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			got[ip]++
		})
	}
	wg.Wait()

	assert.Len(t, got, 3)
	for ip, n := range got {
		assert.Equal(t, 1, n, "%s was handed out twice", ip)
	}
	assert.Equal(t, callers-3, failed)
}

func TestFixedSet_ReleaseFreesTheMemberAndTouchesNothingAtTheProvider(t *testing.T) {
	fake, store, a, _ := fixedSetup(t, 1)
	ip := allocENI(t, a, "eni-1")
	first := store.binding(t, ip).Gen

	require.NoError(t, a.alloc.Release(context.Background(), pool, ip, "eni-1"))
	assert.Len(t, fake.snapshot(), 1, "a member is never deleted")

	again := allocENI(t, a, "eni-2")
	assert.Equal(t, ip, again, "the freed member is handed out again")
	assert.NotEqual(t, first, store.binding(t, again).Gen, "a reused address is a new allocation")
}

func TestFixedSet_ClaimMovesTheMemberToTheOwnersNode(t *testing.T) {
	fake, store, a, b := fixedSetup(t, 1)
	ip := allocENI(t, a, "eni-1")
	a.plug("eni-1")
	_, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	addrID := store.binding(t, ip).AddrID
	require.Equal(t, hostA, fake.holder(addrID))

	a.plugged = map[string]struct{}{}
	b.plug("eni-1")
	res, err := b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Claimed)
	assert.Equal(t, hostB, fake.holder(addrID))
	assert.Equal(t, hostB, store.binding(t, ip).Holder)
}

func TestFixedSet_AnUnownedMemberIsNotSurrendered(t *testing.T) {
	fake, store, a, _ := fixedSetup(t, 1)
	ip, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP, AllocationID: "eipalloc-1"})
	require.NoError(t, err)
	addrID := store.binding(t, ip).AddrID
	require.NoError(t, fake.Attach(context.Background(), addrID, hostA))
	fake.calls = nil

	_, err = a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, fake.callsWith("detach"), "detach changes nothing at the provider, so the pass must not pretend it did")
	assert.Equal(t, hostA, fake.holder(addrID))
}

func TestFixedSet_ReconcileNeverCollectsAMember(t *testing.T) {
	fake, _, a, _ := fixedSetup(t, 2)
	for range 2 {
		res, err := a.alloc.Reconcile(context.Background())
		require.NoError(t, err)
		assert.Empty(t, res.Collected)
		assert.Equal(t, 2, res.Skipped)
	}
	assert.Equal(t, 0, fake.callsWith("delete"))
}

// A holder pass reads the bindings, then acts. Releasing and re-allocating in between puts the
// same address and provider ID under a different owner, which the pass must not move.
func TestFixedSet_PassPausedAcrossReallocationDoesNotMove(t *testing.T) {
	fake, store, a, b := fixedSetup(t, 1)
	ip := allocENI(t, a, "eni-1")
	b.plug("eni-1")
	fake.onList = func() {
		require.NoError(t, a.alloc.Release(context.Background(), pool, ip, "eni-1"))
		again := allocENI(t, a, "eni-2")
		require.Equal(t, ip, again)
	}

	res, err := b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)
	assert.Empty(t, res.Stalled)
	assert.Equal(t, 0, fake.callsWith("attach"), "the stale pass must not move the new allocation")
	assert.Equal(t, "eni-2", store.binding(t, ip).ENIID)
	assert.Empty(t, store.binding(t, ip).Holder)
}

func TestFixedSet_PassPausedAcrossAGatewayVPCChangeDoesNotMove(t *testing.T) {
	fake, store, a, b := fixedSetup(t, 1)
	ip, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP, AllocationID: "eipalloc-nat"})
	require.NoError(t, err)
	binder := NewGatewayBinder(store, []string{pool})
	require.NoError(t, binder.BindGateway(context.Background(), ip.String(), "vpc-1"))
	b.gateway["vpc-1"] = true
	fake.onList = func() {
		require.NoError(t, binder.BindGateway(context.Background(), ip.String(), "vpc-2"))
	}

	res, err := b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)
	assert.Equal(t, 0, fake.callsWith("attach"))
	assert.Equal(t, "vpc-2", store.binding(t, ip).GatewayVPCID)
}

func TestRecordHolder_IgnoresAStaleAllocation(t *testing.T) {
	_, store, a, _ := fixedSetup(t, 1)
	ip := allocENI(t, a, "eni-1")
	live := store.binding(t, ip)

	stale := live
	stale.Gen = "an-earlier-allocation"
	a.alloc.recordHolder(context.Background(), ip.String(), stale, hostA)
	assert.Empty(t, store.binding(t, ip).Holder)

	a.alloc.recordHolder(context.Background(), ip.String(), live, hostA)
	assert.Equal(t, hostA, store.binding(t, ip).Holder)
}

func TestNew_AFixedSetNeedsNoLabelPrefix(t *testing.T) {
	_, err := New(newFixedFake(1), newMemStore(), Config{Pool: external.ExternalPoolConfig{Name: pool}, HostID: hostA})
	require.NoError(t, err)
}

func TestFixedSet_EachAllocationGetsItsOwnGen(t *testing.T) {
	_, store, a, _ := fixedSetup(t, 2)
	ip1 := allocENI(t, a, "eni-1")
	ip2 := allocENI(t, a, "eni-2")
	assert.NotEmpty(t, store.binding(t, ip1).Gen)
	assert.NotEqual(t, store.binding(t, ip1).Gen, store.binding(t, ip2).Gen)
}

// A deployment that does not select the latitude pool keeps the records it always wrote.
func TestVultrRecordCarriesNoGen(t *testing.T) {
	_, store, a, _ := setup(t)
	ip := allocENI(t, a, "eni-1")
	assert.Empty(t, store.binding(t, ip).Gen)
	raw, err := json.Marshal(store.binding(t, ip))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "gen")
}

// failingGets lets the first n reads through, then fails every later one.
type failingGets struct {
	Store

	mu   sync.Mutex
	left int
}

func (s *failingGets) Get(ctx context.Context, poolName string) (Record, error) {
	s.mu.Lock()
	ok := s.left > 0
	s.left--
	s.mu.Unlock()
	if !ok {
		return Record{}, errors.New("store unavailable")
	}
	return s.Store.Get(ctx, poolName)
}

// A pass that could not re-read the binding moved nothing, so it must say so: the daemon
// keeps an address's unreachable warning only while it appears in Stalled.
func TestFixedSet_FailedReReadIsStalledNotSuccess(t *testing.T) {
	fake, store, a, _ := fixedSetup(t, 1)
	ip := allocENI(t, a, "eni-1")
	a.plug("eni-1")
	_, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	addrID := store.binding(t, ip).AddrID

	flaky := &failingGets{Store: store, left: 1}
	b := newFixedNode(t, fake, flaky, hostB)
	b.plug("eni-1")
	fake.calls = nil

	res, err := b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Stalled, 1)
	assert.Equal(t, ip.String(), res.Stalled[0].PublicIP)
	assert.Contains(t, res.Stalled[0].Reason, "re-read")
	assert.Empty(t, res.Claimed)
	assert.Equal(t, 0, fake.callsWith("attach"))
	assert.Equal(t, hostA, fake.holder(addrID), "nothing moved")
}

func TestClaim_FailedReReadDoesNotStallAVultrClaim(t *testing.T) {
	fake, store, a, _ := setup(t)
	ip := allocENI(t, a, "eni-1")
	a.plug("eni-1")
	_, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	flaky := &failingGets{Store: store, left: 1}
	b := newNode(t, fake, flaky, hostB)
	b.plug("eni-1")

	res, err := b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Stalled)
	assert.Equal(t, []string{ip.String()}, res.Claimed)
}

func TestFixedSet_ReleaseNeverDetachesOrDeletes(t *testing.T) {
	fake, _, a, _ := fixedSetup(t, 1)
	ip := allocENI(t, a, "eni-1")
	require.NoError(t, a.alloc.Release(context.Background(), pool, ip, "eni-1"))
	assert.Equal(t, 0, fake.callsWith("detach"))
	assert.Equal(t, 0, fake.callsWith("delete"))
}

func TestFixedSet_ReconcileNeverDetachesOrDeletesAndStillReportsMissing(t *testing.T) {
	fake, store, a, _ := fixedSetup(t, 2)
	// A member the provider no longer lists, bound in the record.
	ip := allocENI(t, a, "eni-1")
	delete(fake.addrs, store.binding(t, ip).AddrID)
	// An unbound member that happens to carry a matching label.
	fake.addrs["eip-2"].Label = prefix + ":stray"

	for range 3 {
		res, err := a.alloc.Reconcile(context.Background())
		require.NoError(t, err)
		assert.Empty(t, res.Collected)
		assert.Equal(t, []string{ip.String()}, res.Missing)
	}
	assert.Equal(t, 0, fake.callsWith("detach"))
	assert.Equal(t, 0, fake.callsWith("delete"))
}
