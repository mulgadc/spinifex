package exonet

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

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/exoscale"
)

const (
	nodeID  = "11111111-1111-4111-8111-111111111111"
	otherID = "22222222-2222-4222-8222-222222222222"
	pool    = "exo"
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
	rec := Record{Bindings: map[string]Binding{}}
	maps.Copy(rec.Bindings, r.Bindings)
	return rec, nil
}

func newTest(t *testing.T, quota int) (*PoolAllocator, *exoscale.Fake, *memStore) {
	t.Helper()
	fake := exoscale.NewFake("de-fra-1", quota)
	store := newMemStore()
	a, err := New(fake, store, Config{Pool: external.ExternalPoolConfig{Name: pool, Source: external.SourceExoscale}, InstanceID: nodeID})
	require.NoError(t, err)
	return a, fake, store
}

func binding(t *testing.T, s *memStore, ip netip.Addr) (Binding, bool) {
	t.Helper()
	rec, err := s.Get(context.Background(), pool)
	if errors.Is(err, kvstore.ErrNotFound) {
		return Binding{}, false
	}
	require.NoError(t, err)
	b, ok := rec.Bindings[ip.String()]
	return b, ok
}

func TestAllocate_CreatesAttachesAndRecords(t *testing.T) {
	a, fake, store := newTest(t, 0)
	ip, err := a.Allocate(context.Background(), external.AllocateRequest{PoolName: pool, Purpose: "eip", AllocationID: "eipalloc-1"})
	require.NoError(t, err)

	b, ok := binding(t, store, ip)
	require.True(t, ok)
	assert.Equal(t, nodeID, b.NodeInstanceID)
	assert.Equal(t, "eipalloc-1", b.AllocationID)

	inst, attached := fake.AttachedTo(b.EIPID)
	assert.True(t, attached)
	assert.Equal(t, nodeID, inst)

	eips, err := fake.ListElasticIPs(context.Background())
	require.NoError(t, err)
	require.Len(t, eips, 1)
	assert.Equal(t, "spinifex:"+nodeID+":eipalloc-1", eips[0].Description)
}

func TestAllocate_QuotaMapsToTheAWSCodeForThePurpose(t *testing.T) {
	cases := []struct{ purpose, want string }{
		{"eip", awserrors.ErrorAddressLimitExceeded},
		{"eni-public", awserrors.ErrorInsufficientAddressCapacity},
	}
	for _, tc := range cases {
		t.Run(tc.purpose, func(t *testing.T) {
			a, fake, _ := newTest(t, 1)
			fake.Seed(exoscale.ElasticIP{ID: "operator", Address: netip.MustParseAddr("89.145.162.53"), Description: "mulga1"})
			_, err := a.Allocate(context.Background(), external.AllocateRequest{Purpose: tc.purpose})
			require.Error(t, err)
			code, ok := awserrors.ResolveErrorCode(err)
			require.True(t, ok, "no AWS code in %v", err)
			assert.Equal(t, tc.want, code)
		})
	}
}

func TestAllocate_AttachFailureDeletesTheNewEIP(t *testing.T) {
	a, fake, store := newTest(t, 0)
	fake.Fail["attach"] = errors.New("boom")
	_, err := a.Allocate(context.Background(), external.AllocateRequest{Purpose: "eni-public", ENIID: "eni-1"})
	require.Error(t, err)
	assert.Empty(t, fake.IDs())
	_, err = store.Get(context.Background(), pool)
	assert.ErrorIs(t, err, kvstore.ErrNotFound)
}

func TestAllocate_RecordFailureDetachesAndDeletes(t *testing.T) {
	a, fake, store := newTest(t, 0)
	store.mutateErr = errors.New("kv down")
	_, err := a.Allocate(context.Background(), external.AllocateRequest{Purpose: "eni-public", ENIID: "eni-1"})
	require.Error(t, err)
	assert.Empty(t, fake.IDs())
	assert.Contains(t, fake.Calls, "detach")
}

func TestRelease_DeletesTheEIP(t *testing.T) {
	a, fake, store := newTest(t, 0)
	ctx := context.Background()
	ip, err := a.Allocate(ctx, external.AllocateRequest{Purpose: "eni-public", ENIID: "eni-1"})
	require.NoError(t, err)

	require.NoError(t, a.Release(ctx, pool, ip, "eni-1"))
	assert.Empty(t, fake.IDs())
	_, ok := binding(t, store, ip)
	assert.False(t, ok)
}

func TestRelease_InstanceTeardownKeepsAnElasticIP(t *testing.T) {
	a, fake, store := newTest(t, 0)
	ctx := context.Background()
	ip, err := a.Allocate(ctx, external.AllocateRequest{Purpose: "eip", AllocationID: "eipalloc-1"})
	require.NoError(t, err)
	require.NoError(t, a.Release(ctx, pool, ip, "eni-1")) // scoped: teardown of an associated instance
	assert.Len(t, fake.IDs(), 1)
	_, ok := binding(t, store, ip)
	assert.True(t, ok)

	require.NoError(t, a.Release(ctx, pool, ip, "")) // ReleaseAddress
	assert.Empty(t, fake.IDs())
}

func TestRelease_StaleOwnerIsANoop(t *testing.T) {
	a, fake, _ := newTest(t, 0)
	ctx := context.Background()
	ip, err := a.Allocate(ctx, external.AllocateRequest{Purpose: "eni-public", ENIID: "eni-new"})
	require.NoError(t, err)
	require.NoError(t, a.Release(ctx, pool, ip, "eni-old"))
	assert.Len(t, fake.IDs(), 1)
}

func TestRelease_EIPAlreadyGoneAtExoscaleSucceeds(t *testing.T) {
	a, fake, _ := newTest(t, 0)
	ctx := context.Background()
	ip, err := a.Allocate(ctx, external.AllocateRequest{Purpose: "eni-public", ENIID: "eni-1"})
	require.NoError(t, err)
	for _, id := range fake.IDs() {
		require.NoError(t, fake.DeleteElasticIP(ctx, id))
	}
	require.NoError(t, a.Release(ctx, pool, ip, "eni-1"))
}

func TestRelease_DeleteFailureIsReported(t *testing.T) {
	a, fake, _ := newTest(t, 0)
	ctx := context.Background()
	ip, err := a.Allocate(ctx, external.AllocateRequest{Purpose: "eni-public", ENIID: "eni-1"})
	require.NoError(t, err)
	fake.Fail["delete"] = errors.New("api down")
	err = a.Release(ctx, pool, ip, "eni-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api down")
}

// A crash between create and record leaves a marked EIP with no binding; the
// reconcile must collect it and must leave everything unmarked or foreign alone.
func TestReconcile_CollectsOnlyOurLeaks(t *testing.T) {
	a, fake, store := newTest(t, 0)
	ctx := context.Background()

	kept, err := a.Allocate(ctx, external.AllocateRequest{Purpose: "eni-public", ENIID: "eni-1"})
	require.NoError(t, err)

	leak, err := fake.CreateElasticIP(ctx, "spinifex:"+nodeID+":eni-crashed")
	require.NoError(t, err)
	require.NoError(t, fake.AttachElasticIP(ctx, nodeID, leak.ID))
	fake.Seed(exoscale.ElasticIP{ID: "operator", Address: netip.MustParseAddr("89.145.162.53"), Description: "mulga1"})
	fake.Seed(exoscale.ElasticIP{ID: "peer", Address: netip.MustParseAddr("89.145.162.54"), Description: "spinifex:" + otherID + ":eni-9"})

	res, err := a.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{leak.ID}, res.Collected)
	assert.Equal(t, 2, res.Skipped)
	assert.NotContains(t, fake.IDs(), leak.ID)
	assert.Contains(t, fake.IDs(), "operator")
	assert.Contains(t, fake.IDs(), "peer")
	_, ok := binding(t, store, kept)
	assert.True(t, ok)
}

func TestReconcile_DropsOnlyThisNodesStaleBindings(t *testing.T) {
	a, _, store := newTest(t, 0)
	ctx := context.Background()
	require.NoError(t, store.Mutate(ctx, pool, func(r *Record) (bool, error) {
		r.Bindings["194.182.160.9"] = Binding{EIPID: "gone", NodeInstanceID: nodeID}
		r.Bindings["194.182.160.10"] = Binding{EIPID: "gone-too", NodeInstanceID: otherID}
		return true, nil
	}))

	res, err := a.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"194.182.160.9"}, res.Stale)
	rec, err := store.Get(ctx, pool)
	require.NoError(t, err)
	assert.Contains(t, rec.Bindings, "194.182.160.10")
	assert.NotContains(t, rec.Bindings, "194.182.160.9")
}

func TestReconcile_ListFailureChangesNothing(t *testing.T) {
	a, fake, _ := newTest(t, 0)
	fake.Fail["list"] = errors.New("api down")
	_, err := a.Reconcile(context.Background())
	require.Error(t, err)
	assert.NotContains(t, fake.Calls, "delete")
}
