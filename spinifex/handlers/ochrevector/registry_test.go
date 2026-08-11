package handlers_ochrevector

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	regAccountA = "111111111111"
	regAccountB = "222222222222"
)

func newRegistryTestStore(t *testing.T) *Registry {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return NewRegistry(js)
}

func TestRegistry_ReserveClaimAndCollision(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rec := Record{ID: "idx-one", Name: "first", Dimension: 768, State: StateCreating, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, reg.Reserve(ctx, regAccountA, rec))

	// A second reservation of the same id under the same account loses the
	// single-writer claim.
	err := reg.Reserve(ctx, regAccountA, rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexExists)

	// The same id under a different account is a distinct key and succeeds.
	require.NoError(t, reg.Reserve(ctx, regAccountB, rec))

	got, err := reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, regAccountA, got.AccountID, "Reserve must stamp AccountID from the accountID argument")
	assert.Equal(t, "first", got.Name)
}

func TestRegistry_GetNotFound(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	got, err := reg.Get(ctx, regAccountA, "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRegistry_SetStateTransitions(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rec := Record{ID: "idx-one", Dimension: 768, State: StateCreating, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, reg.Reserve(ctx, regAccountA, rec))

	require.NoError(t, reg.SetState(ctx, regAccountA, "idx-one", StateReady))
	got, err := reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StateReady, got.State)
	assert.True(t, got.UpdatedAt.After(now) || got.UpdatedAt.Equal(now), "SetState must refresh UpdatedAt")

	require.NoError(t, reg.SetState(ctx, regAccountA, "idx-one", StateDeleting))
	got, err = reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StateDeleting, got.State)
}

func TestRegistry_SetStateNotFound(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	err := reg.SetState(ctx, regAccountA, "does-not-exist", StateReady)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexNotFound)
}

func TestRegistry_Delete(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-one", CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, reg.Delete(ctx, regAccountA, "idx-one"))
	got, err := reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Deleting an already-absent record is a no-op success.
	require.NoError(t, reg.Delete(ctx, regAccountA, "idx-one"))
}

func TestRegistry_ListAccountScoped(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-a1", Name: "a1", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-a2", Name: "a2", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, reg.Reserve(ctx, regAccountB, Record{ID: "idx-b1", Name: "b1", CreatedAt: now, UpdatedAt: now}))

	listA, err := reg.List(ctx, regAccountA)
	require.NoError(t, err)
	assert.Len(t, listA, 2)

	listB, err := reg.List(ctx, regAccountB)
	require.NoError(t, err)
	require.Len(t, listB, 1, "account B must not see account A's indexes")
	assert.Equal(t, "b1", listB[0].Name)
}

func TestRegistry_ListAll(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-a1", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, reg.Reserve(ctx, regAccountB, Record{ID: "idx-b1", CreatedAt: now, UpdatedAt: now}))

	all, err := reg.ListAll(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}
