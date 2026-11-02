package floating

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/vultr"
)

const (
	hostA  = "11111111-1111-4111-8111-111111111111"
	hostB  = "22222222-2222-4222-8222-222222222222"
	pool   = "public"
	prefix = "spinifex-test"
)

// memStore mirrors KVStore, including not-found for a pool never written.
type memStore struct {
	mu        sync.Mutex
	records   map[string]Record
	mutateErr error
}

func newMemStore() *memStore { return &memStore{records: map[string]Record{}} }

func (m *memStore) Mutate(_ context.Context, poolName string, fn func(*Record) (bool, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mutateErr != nil {
		return m.mutateErr
	}
	rec := Record{Bindings: map[string]Binding{}}
	maps.Copy(rec.Bindings, m.records[poolName].Bindings)
	changed, err := fn(&rec)
	if err != nil {
		return err
	}
	if changed {
		m.records[poolName] = rec
	}
	return nil
}

func (m *memStore) Get(_ context.Context, poolName string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[poolName]
	if !ok {
		return Record{}, fmt.Errorf("%w: %s", kvstore.ErrNotFound, poolName)
	}
	out := Record{Bindings: map[string]Binding{}}
	maps.Copy(out.Bindings, r.Bindings)
	return out, nil
}

func (m *memStore) binding(t *testing.T, ip netip.Addr) Binding {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.records[pool].Bindings[ip.String()]
	require.True(t, ok, "no binding for %s", ip)
	return b
}

// node is one allocator on hostID sharing a fake and a store with its peers.
type node struct {
	alloc   *PoolAllocator
	plugged map[string]struct{}
	gateway map[string]bool
}

func newNode(t *testing.T, fake *vultr.Fake, store Store, hostID string) *node {
	t.Helper()
	n := &node{plugged: map[string]struct{}{}, gateway: map[string]bool{}}
	alloc, err := New(NewVultrProvider(fake), store, Config{
		Pool:   external.ExternalPoolConfig{Name: pool, Source: external.SourceVultr, LabelPrefix: prefix},
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

func (n *node) plug(eniID string) { n.plugged[topology.Port(eniID)] = struct{}{} }

func setup(t *testing.T) (*vultr.Fake, *memStore, *node, *node) {
	t.Helper()
	fake := vultr.NewFake("ord", 0, hostA, hostB)
	store := newMemStore()
	return fake, store, newNode(t, fake, store, hostA), newNode(t, fake, store, hostB)
}

func allocENI(t *testing.T, n *node, eniID string) netip.Addr {
	t.Helper()
	ip, err := n.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: "eni-public", ENIID: eniID})
	require.NoError(t, err)
	return ip
}

func TestAllocate_RecordsWithoutAttaching(t *testing.T) {
	fake, store, a, _ := setup(t)
	ip, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP, AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	b := store.binding(t, ip)
	assert.Equal(t, "eipalloc-1", b.AllocationID)
	assert.Empty(t, fake.HolderOf(b.AddrID), "the owner's node attaches, never the allocating node")
	got, err := fake.GetReservedIP(context.Background(), b.AddrID)
	require.NoError(t, err)
	assert.Equal(t, prefix+":eipalloc-1", got.Label)
}

func TestAllocate_RecordFailureDeletesTheAddress(t *testing.T) {
	fake, store, a, _ := setup(t)
	store.mutateErr = errors.New("kv down")
	_, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, ENIID: "eni-1"})
	require.Error(t, err)
	assert.Empty(t, fake.IDs())
}

func TestAllocate_QuotaMapsToTheAWSCodeForTheCall(t *testing.T) {
	fake := vultr.NewFake("ord", 1, hostA)
	a := newNode(t, fake, newMemStore(), hostA)
	allocENI(t, a, "eni-1")

	_, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP})
	assert.ErrorContains(t, err, awserrors.ErrorAddressLimitExceeded)
	_, err = a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: "eni-public"})
	assert.ErrorContains(t, err, awserrors.ErrorInsufficientAddressCapacity)
}

func TestRelease_InstanceTeardownKeepsAnElasticIP(t *testing.T) {
	fake, store, a, _ := setup(t)
	ip, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP, AllocationID: "eipalloc-1"})
	require.NoError(t, err)
	require.NoError(t, a.alloc.BindOwner(context.Background(), pool, ip, "eni-1"))

	require.NoError(t, a.alloc.Release(context.Background(), pool, ip, "eni-1"))
	store.binding(t, ip)
	assert.Len(t, fake.IDs(), 1)

	require.NoError(t, a.alloc.Release(context.Background(), pool, ip, ""))
	assert.Empty(t, fake.IDs())
}

func TestRelease_DeletesAnAttachedAddress(t *testing.T) {
	fake, _, a, _ := setup(t)
	ip := allocENI(t, a, "eni-1")
	a.plug("eni-1")
	_, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)

	require.NoError(t, a.alloc.Release(context.Background(), pool, ip, "eni-1"))
	assert.Empty(t, fake.IDs())
}

func TestRelease_DetachesBeforeDeleting(t *testing.T) {
	fake, _, a, _ := setup(t)
	fake.RefuseDeleteAttached = true
	ip := allocENI(t, a, "eni-1")
	a.plug("eni-1")
	_, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)

	require.NoError(t, a.alloc.Release(context.Background(), pool, ip, "eni-1"))
	assert.Empty(t, fake.IDs(), "an attached address must still be deleted when Vultr refuses an implicit detach")
}

func TestRelease_StaleOwnerIsANoop(t *testing.T) {
	fake, _, a, _ := setup(t)
	ip := allocENI(t, a, "eni-1")
	require.NoError(t, a.alloc.Release(context.Background(), pool, ip, "eni-other"))
	assert.Len(t, fake.IDs(), 1)
}

func TestClaim_AttachesWhereTheOwnerRuns(t *testing.T) {
	fake, store, a, b := setup(t)
	// Allocated by whichever node served the request, run on the other.
	ip := allocENI(t, a, "eni-1")
	b.plug("eni-1")

	res, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.Local, "the allocating node does not run the owner")

	res, err = b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Claimed)
	assert.Empty(t, res.Stalled)
	addrID := store.binding(t, ip).AddrID
	assert.Equal(t, hostB, fake.HolderOf(addrID))
	assert.Equal(t, hostB, store.binding(t, ip).Holder)

	fake.Calls = nil
	res, err = b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Claimed)
	assert.Equal(t, 1, res.Local)
	assert.Equal(t, []string{"list"}, fake.Calls, "a settled address costs one listing and no writes")
}

func TestClaim_FollowsTheOwnerToAnotherNode(t *testing.T) {
	fake, store, a, b := setup(t)
	ip := allocENI(t, a, "eni-1")
	a.plug("eni-1")
	_, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)

	// The guest is recovered on B; A is dead and never runs another pass.
	b.plug("eni-1")
	res, err := b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Claimed)
	assert.Equal(t, hostB, fake.HolderOf(store.binding(t, ip).AddrID))
}

func TestClaim_AttachFailureIsStalledNotFatal(t *testing.T) {
	fake, _, a, _ := setup(t)
	ip1 := allocENI(t, a, "eni-1")
	ip2 := allocENI(t, a, "eni-2")
	a.plug("eni-1")
	a.plug("eni-2")
	fake.Fail["attach"] = errors.New("throttled")

	res, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Stalled, 1)
	require.Len(t, res.Claimed, 1)
	assert.ElementsMatch(t, []string{ip1.String(), ip2.String()}, []string{res.Stalled[0].PublicIP, res.Claimed[0]})
	assert.NotEmpty(t, res.Stalled[0].ENIID, "the daemon names the guest from the ENI")
}

func TestClaim_DisassociatedAddressIsDetached(t *testing.T) {
	fake, store, a, _ := setup(t)
	ip, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP, AllocationID: "eipalloc-1"})
	require.NoError(t, err)
	require.NoError(t, a.alloc.BindOwner(context.Background(), pool, ip, "eni-1"))
	a.plug("eni-1")
	_, err = a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)

	require.NoError(t, a.alloc.BindOwner(context.Background(), pool, ip, ""))
	_, err = a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, fake.HolderOf(store.binding(t, ip).AddrID))
	assert.Empty(t, store.binding(t, ip).Holder)
	assert.Len(t, fake.IDs(), 1, "a disassociated Elastic IP stays allocated")
}

func TestClaim_NATGatewayAddressFollowsTheChassis(t *testing.T) {
	fake, store, a, b := setup(t)
	ip, err := a.alloc.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: purposeEIP, AllocationID: "eipalloc-nat"})
	require.NoError(t, err)
	require.NoError(t, NewGatewayBinder(store, []string{pool}).BindGateway(context.Background(), ip.String(), "vpc-1"))
	b.gateway["vpc-1"] = true

	res, err := b.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Claimed)
	assert.Equal(t, hostB, fake.HolderOf(store.binding(t, ip).AddrID))
}

func TestClaim_UnreadableBindingsMarkNothing(t *testing.T) {
	_, _, a, _ := setup(t)
	res, err := a.alloc.ClaimLocalAddresses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, external.ClaimResult{}, res)
}

func TestReconcile_CollectsALeakOnlyOnTheSecondSighting(t *testing.T) {
	fake, _, a, _ := setup(t)
	leak, err := fake.CreateReservedIP(context.Background(), prefix+":eni-crashed")
	require.NoError(t, err)
	fake.Seed(vultr.ReservedIP{ID: "operator-own", Label: "website", Address: netip.MustParseAddr("203.0.113.9")})
	fake.Seed(vultr.ReservedIP{ID: "other-deployment", Label: prefix + "-two:eni-9", Address: netip.MustParseAddr("203.0.113.10")})
	allocENI(t, a, "eni-1")

	res, err := a.alloc.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Collected, "first sight could be another node between create and record")
	assert.Equal(t, 1, res.Suspect)
	assert.Equal(t, 2, res.Skipped)

	res, err = a.alloc.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{leak.ID}, res.Collected)
	assert.ElementsMatch(t, []string{"operator-own", "other-deployment", "fake-rip-0002"}, fake.IDs())
}

func TestReconcile_AnAddressRecordedBetweenPassesIsKept(t *testing.T) {
	fake, store, a, _ := setup(t)
	addr, err := fake.CreateReservedIP(context.Background(), prefix+":eni-1")
	require.NoError(t, err)
	_, err = a.alloc.Reconcile(context.Background())
	require.NoError(t, err)

	require.NoError(t, store.Mutate(context.Background(), pool, func(r *Record) (bool, error) {
		r.Bindings[addr.Address.String()] = Binding{AddrID: addr.ID, ENIID: "eni-1"}
		return true, nil
	}))
	res, err := a.alloc.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Collected)
	assert.Len(t, fake.IDs(), 1)
}

func TestReconcile_ReportsAnAddressDeletedBehindOurBack(t *testing.T) {
	fake, store, a, _ := setup(t)
	ip := allocENI(t, a, "eni-1")
	require.NoError(t, fake.DeleteReservedIP(context.Background(), store.binding(t, ip).AddrID))

	res, err := a.alloc.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{ip.String()}, res.Missing)
	store.binding(t, ip)
}

func TestNew_RefusesAPoolWithoutHostOrPrefix(t *testing.T) {
	fake := vultr.NewFake("ord", 0)
	_, err := New(NewVultrProvider(fake), newMemStore(), Config{Pool: external.ExternalPoolConfig{Name: pool, LabelPrefix: prefix}})
	require.Error(t, err)
	_, err = New(NewVultrProvider(fake), newMemStore(), Config{Pool: external.ExternalPoolConfig{Name: pool}, HostID: hostA})
	require.Error(t, err)
}
