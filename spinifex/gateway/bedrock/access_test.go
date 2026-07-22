package gateway_bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shipped catalog entries, one per tier, referenced by the access tests.
// Naming them here keeps a catalog change to one edit per tier rather than one
// per assertion.
const (
	selfHostTestModel  = "meta.llama3-70b-instruct-v1:0"
	anthropicTestModel = "anthropic.claude-3-5-sonnet-20240620-v1:0"
)

// grantSet is an AccessResolver granting exactly the model IDs it contains.
// The shipped routers are exercised through it so a test can express "this
// account may use that model" without standing up JetStream.
type grantSet map[string]bool

var _ AccessResolver = grantSet(nil)

func (g grantSet) Granted(_ context.Context, _, modelID string) (bool, error) {
	return g[modelID], nil
}

// grantAll is an AccessResolver granting every model, for tests whose subject
// is something other than access control.
type grantAll struct{}

var _ AccessResolver = grantAll{}

func (grantAll) Granted(_ context.Context, _, _ string) (bool, error) { return true, nil }

// failingAccess reports an error from every check, covering the path where the
// grant store itself is unhealthy.
type failingAccess struct{}

var _ AccessResolver = failingAccess{}

func (failingAccess) Granted(_ context.Context, _, _ string) (bool, error) {
	return false, errors.New("kv unavailable")
}

func TestDenyAllAccessResolver_GrantsNothing(t *testing.T) {
	for _, modelID := range []string{selfHostTestModel, anthropicTestModel, "nonexistent.model-v1:0"} {
		granted, err := DenyAllAccessResolver.Granted(context.Background(), "000000000001", modelID)
		require.NoError(t, err)
		assert.False(t, granted, "model %q must not be granted by the deny-all fallback", modelID)
	}
}

func TestAccessKey_EncodesModelID(t *testing.T) {
	// Model IDs contain ':', which NATS rejects in a KV key, so the model
	// segment must be encoded rather than interpolated raw.
	key := accessKey("000000000001", "anthropic.claude-3-5-sonnet-20240620-v1:0")
	assert.NotContains(t, key, ":")
	assert.Greater(t, len(key), len(accountGrantPrefix("000000000001")))
}

// TestModelAccessStore_GrantResolveRevoke exercises the real JetStream KV
// path: lazy bucket create, Grant, the hit and miss branches of Granted, and
// Revoke.
func TestModelAccessStore_GrantResolveRevoke(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewModelAccessStore(js, 1)
	ctx := context.Background()

	granted, err := store.Granted(ctx, "000000000001", selfHostTestModel)
	require.NoError(t, err)
	assert.False(t, granted, "a model must be denied before it is granted")

	require.NoError(t, store.Grant(ctx, "000000000001", selfHostTestModel))
	granted, err = store.Granted(ctx, "000000000001", selfHostTestModel)
	require.NoError(t, err)
	assert.True(t, granted)

	// A grant is scoped to one account and one model.
	granted, err = store.Granted(ctx, "000000000002", selfHostTestModel)
	require.NoError(t, err)
	assert.False(t, granted, "a grant must not leak to another account")
	granted, err = store.Granted(ctx, "000000000001", anthropicTestModel)
	require.NoError(t, err)
	assert.False(t, granted, "a grant must not leak to another model")

	require.NoError(t, store.Revoke(ctx, "000000000001", selfHostTestModel))
	granted, err = store.Granted(ctx, "000000000001", selfHostTestModel)
	require.NoError(t, err)
	assert.False(t, granted)
}

func TestModelAccessStore_GrantIsIdempotent(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewModelAccessStore(js, 1)
	ctx := context.Background()

	require.NoError(t, store.Grant(ctx, "000000000001", selfHostTestModel))
	require.NoError(t, store.Grant(ctx, "000000000001", selfHostTestModel))

	models, err := store.List(ctx, "000000000001")
	require.NoError(t, err)
	assert.Equal(t, []string{selfHostTestModel}, models, "a repeated grant must not duplicate the entry")
}

// TestModelAccessStore_RevokeMissingGrantSucceeds covers the ErrKeyNotFound
// branch: callers revoke without checking first, so a missing grant is not an
// error.
func TestModelAccessStore_RevokeMissingGrantSucceeds(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewModelAccessStore(js, 1)

	assert.NoError(t, store.Revoke(context.Background(), "000000000001", selfHostTestModel))
}

// TestModelAccessStore_List_RoundTripsModelIDs proves the key encoding is
// reversible, including the ':' that forced it.
func TestModelAccessStore_List_RoundTripsModelIDs(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewModelAccessStore(js, 1)
	ctx := context.Background()

	require.NoError(t, store.Grant(ctx, "000000000001", selfHostTestModel))
	require.NoError(t, store.Grant(ctx, "000000000001", anthropicTestModel))
	require.NoError(t, store.Grant(ctx, "000000000002", selfHostTestModel))

	models, err := store.List(ctx, "000000000001")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{selfHostTestModel, anthropicTestModel}, models)

	models, err = store.List(ctx, "000000000002")
	require.NoError(t, err)
	assert.Equal(t, []string{selfHostTestModel}, models, "List must not return another account's grants")
}

func TestModelAccessStore_ListEmptyAccount(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewModelAccessStore(js, 1)

	models, err := store.List(context.Background(), "000000000009")
	require.NoError(t, err)
	assert.Empty(t, models)
}

// TestModelAccessStore_SystemAccountBypassesGrants mirrors how
// handlers_quota exempts the system account from every quota dimension.
func TestModelAccessStore_SystemAccountBypassesGrants(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewModelAccessStore(js, 1)

	granted, err := store.Granted(context.Background(), utils.GlobalAccountID, selfHostTestModel)
	require.NoError(t, err)
	assert.True(t, granted, "the system account must bypass grants")
}

// TestGrantedCatalogEntry_ErrorClasses pins the distinction the runtime paths
// depend on: an unknown model is not found, a known-but-ungranted model is
// access denied, and a broken grant store propagates rather than failing open.
func TestGrantedCatalogEntry_ErrorClasses(t *testing.T) {
	ctx := context.Background()

	_, err := grantedCatalogEntry(ctx, "000000000001", "nonexistent.model-v1:0", grantAll{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())

	_, err = grantedCatalogEntry(ctx, "000000000001", selfHostTestModel, grantSet{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())

	_, err = grantedCatalogEntry(ctx, "000000000001", selfHostTestModel, failingAccess{})
	require.Error(t, err)
	assert.Equal(t, "kv unavailable", err.Error())

	entry, err := grantedCatalogEntry(ctx, "000000000001", selfHostTestModel, grantSet{selfHostTestModel: true})
	require.NoError(t, err)
	assert.Equal(t, selfHostTestModel, entry.ModelID)
}
