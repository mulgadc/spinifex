package gateway_bedrock

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	priceTestSelfHostModelID = "meta.llama3-2-1b-instruct-v1:0"
	priceTestProviderModelID = "anthropic.claude-3-5-sonnet-20240620-v1:0"
	priceTestUnknownModelID  = "no-such-model"
)

// stubPriceResolver returns a fixed (price, ok, err) for every Resolve call,
// standing in for a KV override without a real JetStream bucket.
type stubPriceResolver struct {
	price Price
	ok    bool
	err   error
}

func (s stubPriceResolver) Resolve(context.Context, string) (Price, bool, error) {
	return s.price, s.ok, s.err
}

// TestResolvePrice_SelfHost_AlwaysKnownZero asserts D12's hard rule: a
// self-hosted model's external cost is always a *known* zero — never
// consulted from the catalog's (irrelevant, zero-value) price fields — so it
// can never be confused with an unpriced model.
func TestResolvePrice_SelfHost_AlwaysKnownZero(t *testing.T) {
	entry, ok := lookupCatalogEntry(priceTestSelfHostModelID)
	require.True(t, ok)

	// A KV override present for this model must still be ignored: self-host
	// pricing is a hard rule, not a resolvable-but-currently-zero default.
	price, err := resolvePrice(context.Background(), stubPriceResolver{price: Price{InputMicroUSDPerMillion: 999, Known: true}, ok: true}, entry)
	require.NoError(t, err)
	assert.True(t, price.Known)
	assert.Zero(t, price.InputMicroUSDPerMillion)
	assert.Zero(t, price.OutputMicroUSDPerMillion)
}

// TestResolvePrice_ProviderUsesInTreeDefault covers a provider entry with no
// KV override: it falls back to the catalog's in-tree default price.
func TestResolvePrice_ProviderUsesInTreeDefault(t *testing.T) {
	entry, ok := lookupCatalogEntry(priceTestProviderModelID)
	require.True(t, ok)

	price, err := resolvePrice(context.Background(), nil, entry)
	require.NoError(t, err)
	assert.True(t, price.Known)
	assert.Equal(t, entry.InputPriceMicroUSDPerMillion, price.InputMicroUSDPerMillion)
	assert.Equal(t, entry.OutputPriceMicroUSDPerMillion, price.OutputMicroUSDPerMillion)
}

// TestResolvePrice_KVOverrideWinsOverInTreeDefault covers a provider entry
// with a KV override present: the override wins outright, not just fills a
// gap in the in-tree default.
func TestResolvePrice_KVOverrideWinsOverInTreeDefault(t *testing.T) {
	entry, ok := lookupCatalogEntry(priceTestProviderModelID)
	require.True(t, ok)

	override := Price{InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, Known: true}
	price, err := resolvePrice(context.Background(), stubPriceResolver{price: override, ok: true}, entry)
	require.NoError(t, err)
	assert.Equal(t, override, price)
	assert.NotEqual(t, entry.InputPriceMicroUSDPerMillion, price.InputMicroUSDPerMillion)
}

// TestResolvePrice_NoDefaultNoOverride_Unknown is the D12 invariant under
// test: a model with neither a KV override nor an in-tree default resolves
// Known=false, never a silent zero.
func TestResolvePrice_NoDefaultNoOverride_Unknown(t *testing.T) {
	entry := catalogEntry{ModelID: priceTestUnknownModelID, Provider: providerPrefix + "unpriced-vendor"}

	price, err := resolvePrice(context.Background(), nil, entry)
	require.NoError(t, err)
	assert.False(t, price.Known)
}

// TestResolvePrice_ResolverErrorPropagates ensures a genuine KV read failure
// surfaces rather than silently falling through to the in-tree default or an
// unknown price — an internal fault is not a pricing verdict.
func TestResolvePrice_ResolverErrorPropagates(t *testing.T) {
	entry, ok := lookupCatalogEntry(priceTestProviderModelID)
	require.True(t, ok)

	_, err := resolvePrice(context.Background(), stubPriceResolver{err: assert.AnError}, entry)
	require.Error(t, err)
}

// TestPriceStore_PutGetDelete exercises the real JetStream KV path, mirroring
// TestWeightsStore_PutAndResolve_KV.
func TestPriceStore_PutGetDelete(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := NewPriceStore(testutil.NewJetStream(t, nc), 1)
	ctx := context.Background()

	_, ok, err := store.Resolve(ctx, priceTestProviderModelID)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, store.PutPrice(ctx, priceTestProviderModelID, Price{InputMicroUSDPerMillion: 5_000_000, OutputMicroUSDPerMillion: 10_000_000}))

	got, ok, err := store.Resolve(ctx, priceTestProviderModelID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, got.Known, "PutPrice must always store a known price")
	assert.EqualValues(t, 5_000_000, got.InputMicroUSDPerMillion)
	assert.EqualValues(t, 10_000_000, got.OutputMicroUSDPerMillion)

	require.NoError(t, store.DeletePrice(ctx, priceTestProviderModelID))
	_, ok, err = store.Resolve(ctx, priceTestProviderModelID)
	require.NoError(t, err)
	assert.False(t, ok)

	// Deleting an already-absent override is idempotent, not an error.
	require.NoError(t, store.DeletePrice(ctx, priceTestProviderModelID))
}
