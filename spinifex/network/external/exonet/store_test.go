package exonet

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/exoscale"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/testutil"
)

func TestKVStore_UnwrittenPoolIsNotFound(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := NewKVStore(js).Get(context.Background(), pool)
	assert.ErrorIs(t, err, kvstore.ErrNotFound)
}

// A daemon restart builds a new allocator over the same bucket. Its reconcile
// must find the live binding and leave the EIP alone, not collect it as a leak.
func TestKVStore_BindingSurvivesARestartReconcile(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	fake := exoscale.NewFake("de-fra-1", 0)
	cfg := Config{Pool: external.ExternalPoolConfig{Name: pool, Source: external.SourceExoscale}, InstanceID: nodeID}

	first, err := New(fake, NewKVStore(js), cfg)
	require.NoError(t, err)
	ip, err := first.Allocate(ctx, external.AllocateRequest{PoolName: pool, Purpose: "eni-public", ENIID: "eni-1"})
	require.NoError(t, err)

	second, err := New(fake, NewKVStore(js), cfg)
	require.NoError(t, err)
	res, err := second.Reconcile(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Collected)
	assert.Empty(t, res.Stale)
	assert.Len(t, fake.IDs(), 1)

	require.NoError(t, second.Release(ctx, pool, ip, "eni-1"))
	assert.Empty(t, fake.IDs())
	rec, err := NewKVStore(js).Get(ctx, pool)
	require.NoError(t, err)
	assert.NotContains(t, rec.Bindings, ip.String())
}

func TestFromPoolConfig_RefusesAnUnusableBinary(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := FromPoolConfig(context.Background(), js, external.ExternalPoolConfig{
		Name: pool, Source: external.SourceExoscale, ExoscaleZone: "de-fra-1",
		ExoscaleInstanceID: nodeID, ExoscaleBinary: "/bin/false",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exo CLI is not usable")
}

func TestFromPoolConfig_RefusesAnotherSource(t *testing.T) {
	_, err := FromPoolConfig(context.Background(), nil, external.ExternalPoolConfig{Name: pool, Source: "static"})
	require.Error(t, err)
}
