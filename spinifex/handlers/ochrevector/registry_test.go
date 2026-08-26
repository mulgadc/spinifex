// Exercises the unexported registry internals with no exported surface
// to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"context"
	"fmt"
	"sync"
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

// TestRegistry_PurgeAll proves PurgeAll clears every record across every
// account, not just one -- the appliance-teardown use case it exists for.
func TestRegistry_PurgeAll(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, reg.Reserve(ctx, regAccountB, Record{ID: "idx-two", CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, reg.PurgeAll(ctx))

	all, err := reg.ListAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, all)
}

// TestRegistry_PurgeAllOnEmptyBucketIsANoOp proves PurgeAll succeeds against
// a registry with no records yet -- a bucket kv.Keys reports as ErrNoKeysFound.
func TestRegistry_PurgeAllOnEmptyBucketIsANoOp(t *testing.T) {
	reg := newRegistryTestStore(t)
	require.NoError(t, reg.PurgeAll(context.Background()))
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

func TestRegistry_AppendSourceSpecIsIdempotent(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-one", CreatedAt: now, UpdatedAt: now}))

	spec := SourceSpec{Bucket: "docs", Prefix: "kb/", ChunkSize: 512, ChunkOverlap: 64, EmbeddingModel: "nomic-embed-text-v1.5", Dimension: 768}
	require.NoError(t, reg.AppendSourceSpec(ctx, regAccountA, "idx-one", spec))

	got, err := reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.SourceSpecs, 1)
	assert.Equal(t, spec, got.SourceSpecs[0])

	// Appending the identical spec again must not duplicate it.
	require.NoError(t, reg.AppendSourceSpec(ctx, regAccountA, "idx-one", spec))
	got, err = reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.Len(t, got.SourceSpecs, 1)

	// A distinct spec (different prefix) is a second entry.
	other := spec
	other.Prefix = "kb2/"
	require.NoError(t, reg.AppendSourceSpec(ctx, regAccountA, "idx-one", other))
	got, err = reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.Len(t, got.SourceSpecs, 2)
}

func TestRegistry_AppendSourceSpecNotFound(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	err := reg.AppendSourceSpec(ctx, regAccountA, "does-not-exist", SourceSpec{Bucket: "docs"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexNotFound)
}

// TestRegistry_AppendSourceSpecConcurrentAppendsAllLand is the case that can
// lose data: the read-check-append is not atomic, so two ingests racing on one
// index must not drop a spec. Every distinct spec has to survive.
func TestRegistry_AppendSourceSpecConcurrentAppendsAllLand(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-one", CreatedAt: now, UpdatedAt: now}))

	const writers = 4
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			spec := SourceSpec{Bucket: "docs", Prefix: fmt.Sprintf("kb%d/", i), ChunkSize: 512, Dimension: 768}
			errs[i] = reg.AppendSourceSpec(ctx, regAccountA, "idx-one", spec)
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "appender %d", i)
	}

	got, err := reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Len(t, got.SourceSpecs, writers, "a spec was dropped by a lost CAS race")

	prefixes := make(map[string]bool, writers)
	for _, spec := range got.SourceSpecs {
		prefixes[spec.Prefix] = true
	}
	for i := range writers {
		assert.Truef(t, prefixes[fmt.Sprintf("kb%d/", i)], "appender %d's spec is missing", i)
	}
}

// TestRegistry_SetStateConcurrentWritersAllSucceed pins the retry: before the
// conversion this was a single-shot update at the read revision, so a caller
// that lost the race got a raw revision mismatch back.
func TestRegistry_SetStateConcurrentWritersAllSucceed(t *testing.T) {
	reg := newRegistryTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, reg.Reserve(ctx, regAccountA, Record{ID: "idx-one", State: StateCreating, CreatedAt: now, UpdatedAt: now}))

	states := []string{StateReady, StateStale, StateReady, StateDeleting}
	var wg sync.WaitGroup
	errs := make([]error, len(states))
	for i, state := range states {
		wg.Go(func() {
			errs[i] = reg.SetState(ctx, regAccountA, "idx-one", state)
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "writer %d lost its race instead of retrying", i)
	}

	got, err := reg.Get(ctx, regAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, states, got.State, "the surviving state must be one a writer actually set")
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
