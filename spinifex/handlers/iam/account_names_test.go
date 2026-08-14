package handlers_iam_test

import (
	"strings"
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAccountNameIndex(t *testing.T) (*handlers_iam.AccountNameIndex, jetstream.JetStream) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	index, err := handlers_iam.NewAccountNameIndex(t.Context(), js)
	require.NoError(t, err)
	return index, js
}

func TestAccountNameIndexReserveCommitLookup(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	ctx := t.Context()

	_, found, err := index.Lookup(ctx, "ben@example.com")
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, index.Reserve(ctx, "ben@example.com", "token-a"))

	// An uncommitted reservation names no account yet.
	_, found, err = index.Lookup(ctx, "ben@example.com")
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, index.Commit(ctx, "ben@example.com", "000000000042", "token-a"))

	accountID, found, err := index.Lookup(ctx, "ben@example.com")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "000000000042", accountID)
}

func TestAccountNameIndexReserveConflicts(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	ctx := t.Context()

	require.NoError(t, index.Reserve(ctx, "ben@example.com", "token-a"))

	// The same token resuming its own attempt must proceed.
	require.NoError(t, index.Reserve(ctx, "ben@example.com", "token-a"))

	err := index.Reserve(ctx, "ben@example.com", "token-b")
	assert.ErrorIs(t, err, handlers_iam.ErrNameInFlight)

	require.NoError(t, index.Commit(ctx, "ben@example.com", "000000000042", "token-a"))

	err = index.Reserve(ctx, "ben@example.com", "token-b")
	assert.ErrorIs(t, err, handlers_iam.ErrNameTaken)
}

// Case and whitespace must not create a second account for the same person.
func TestAccountNameIndexNormalizesName(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	ctx := t.Context()

	require.NoError(t, index.Reserve(ctx, "ben@example.com", "token-a"))
	require.NoError(t, index.Commit(ctx, "ben@example.com", "000000000042", "token-a"))

	accountID, found, err := index.Lookup(ctx, "  BEN@Example.COM ")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "000000000042", accountID)

	assert.ErrorIs(t, index.Reserve(ctx, "BEN@EXAMPLE.COM", "token-b"), handlers_iam.ErrNameTaken)
}

func TestAccountNameIndexRelease(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	ctx := t.Context()

	require.NoError(t, index.Reserve(ctx, "ben@example.com", "token-a"))

	// A reservation belongs to its owner; another token cannot drop it.
	require.NoError(t, index.Release(ctx, "ben@example.com", "token-b"))
	assert.ErrorIs(t, index.Reserve(ctx, "ben@example.com", "token-b"), handlers_iam.ErrNameInFlight)

	require.NoError(t, index.Release(ctx, "ben@example.com", "token-a"))
	require.NoError(t, index.Reserve(ctx, "ben@example.com", "token-b"))

	// Releasing a committed entry would unindex a live account.
	require.NoError(t, index.Commit(ctx, "ben@example.com", "000000000042", "token-b"))
	require.NoError(t, index.Release(ctx, "ben@example.com", "token-b"))
	_, found, err := index.Lookup(ctx, "ben@example.com")
	require.NoError(t, err)
	assert.True(t, found)
}

// Release on a name that was never reserved is a no-op, not an error: it runs
// on the failure path where the reservation may already be gone.
func TestAccountNameIndexReleaseUnknownName(t *testing.T) {
	index, _ := newAccountNameIndex(t)
	require.NoError(t, index.Release(t.Context(), "nobody@example.com", "token-a"))
}

// KV keys are readable by anyone with cluster access, so a customer's email
// address must not appear in one.
func TestAccountNameIndexKeysCarryNoEmailAddress(t *testing.T) {
	index, js := newAccountNameIndex(t)
	ctx := t.Context()

	require.NoError(t, index.Reserve(ctx, "ben@example.com", "token-a"))

	kv, err := js.KeyValue(ctx, handlers_iam.KVBucketAccountNames)
	require.NoError(t, err)
	keys, err := kv.Keys(ctx)
	require.NoError(t, err)
	require.Len(t, keys, 1)

	assert.NotContains(t, keys[0], "ben")
	assert.NotContains(t, keys[0], "@")
	assert.Len(t, keys[0], 64, "key should be a hex SHA-256 digest")
	assert.Equal(t, strings.ToLower(keys[0]), keys[0])
}

func TestFindAccountByName(t *testing.T) {
	svc := newIAMService(t)

	created, err := svc.CreateAccount("ben@example.com")
	require.NoError(t, err)

	found, err := handlers_iam.FindAccountByName(svc, "  BEN@Example.com ")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, created.AccountID, found.AccountID)

	missing, err := handlers_iam.FindAccountByName(svc, "nobody@example.com")
	require.NoError(t, err)
	assert.Nil(t, missing)
}
