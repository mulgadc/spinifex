package exoscale_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/cloud/exoscale"
)

// The Fake is what every exonet test is measured against, so these pin the
// behaviours of the real CLI that the allocator's rollback and release paths
// depend on.

// The quota is per organisation, so an EIP Spinifex did not make still counts.
func TestFakeQuotaCountsEveryEIPAndUsesTheSentinel(t *testing.T) {
	ctx := context.Background()
	f := exoscale.NewFake("de-fra-1", 2)
	f.Seed(exoscale.ElasticIP{ID: "operator", Address: netip.MustParseAddr("89.145.162.53")})

	_, err := f.CreateElasticIP(ctx, "spinifex:a")
	require.NoError(t, err)
	_, err = f.CreateElasticIP(ctx, "spinifex:b")
	require.ErrorIs(t, err, exoscale.ErrQuotaExceeded)
}

func TestFakeGoneEIPIsNotFoundOnEveryCall(t *testing.T) {
	ctx := context.Background()
	f := exoscale.NewFake("de-fra-1", 0)
	e, err := f.CreateElasticIP(ctx, "spinifex:a")
	require.NoError(t, err)
	require.NoError(t, f.AttachElasticIP(ctx, "inst-1", e.ID))
	require.NoError(t, f.DeleteElasticIP(ctx, e.ID))

	_, attached := f.AttachedTo(e.ID)
	assert.False(t, attached, "deleting an EIP detaches it")
	require.ErrorIs(t, f.AttachElasticIP(ctx, "inst-1", e.ID), exoscale.ErrNotFound)
	require.ErrorIs(t, f.DetachElasticIP(ctx, "inst-1", e.ID), exoscale.ErrNotFound)
	require.ErrorIs(t, f.DeleteElasticIP(ctx, e.ID), exoscale.ErrNotFound)
}

func TestFakeDetachFromTheWrongInstanceIsNotFound(t *testing.T) {
	ctx := context.Background()
	f := exoscale.NewFake("de-fra-1", 0)
	e, err := f.CreateElasticIP(ctx, "spinifex:a")
	require.NoError(t, err)
	require.NoError(t, f.AttachElasticIP(ctx, "inst-1", e.ID))

	require.ErrorIs(t, f.DetachElasticIP(ctx, "inst-2", e.ID), exoscale.ErrNotFound)
	inst, attached := f.AttachedTo(e.ID)
	assert.True(t, attached)
	assert.Equal(t, "inst-1", inst)
}

func TestFakeFailIsOneShot(t *testing.T) {
	ctx := context.Background()
	f := exoscale.NewFake("de-fra-1", 0)
	f.Fail["list"] = assert.AnError

	_, err := f.ListElasticIPs(ctx)
	require.ErrorIs(t, err, assert.AnError)
	_, err = f.ListElasticIPs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"list", "list"}, f.Calls)
}
