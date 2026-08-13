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
	jobAccountA = "111111111111"
	jobAccountB = "222222222222"
)

func newJobStoreTestStore(t *testing.T) *JobStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return NewJobStore(js)
}

func TestJobStore_ReserveClaimAndCollision(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rec := JobRecord{ID: "job-one", IndexID: "idx-one", State: JobStatePending, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.Reserve(ctx, jobAccountA, rec))

	// A second reservation of the same id under the same account loses the
	// single-writer claim.
	err := store.Reserve(ctx, jobAccountA, rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrJobExists)

	// The same id under a different account is a distinct key and succeeds.
	require.NoError(t, store.Reserve(ctx, jobAccountB, rec))

	got, err := store.Get(ctx, jobAccountA, "job-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, jobAccountA, got.AccountID, "Reserve must stamp AccountID from the accountID argument")
	assert.Equal(t, "idx-one", got.IndexID)
}

func TestJobStore_GetNotFound(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	got, err := store.Get(ctx, jobAccountA, "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestJobStore_SetStateTransitions(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rec := JobRecord{ID: "job-one", State: JobStatePending, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.Reserve(ctx, jobAccountA, rec))

	require.NoError(t, store.SetState(ctx, jobAccountA, "job-one", JobStateRunning))
	got, err := store.Get(ctx, jobAccountA, "job-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateRunning, got.State)
	assert.True(t, got.UpdatedAt.After(now) || got.UpdatedAt.Equal(now), "SetState must refresh UpdatedAt")

	require.NoError(t, store.SetState(ctx, jobAccountA, "job-one", JobStateReady))
	got, err = store.Get(ctx, jobAccountA, "job-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateReady, got.State)
}

func TestJobStore_SetStateNotFound(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	err := store.SetState(ctx, jobAccountA, "does-not-exist", JobStateRunning)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrJobNotFound)
}

func TestJobStore_Update(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, store.Reserve(ctx, jobAccountA, JobRecord{ID: "job-one", CreatedAt: now, UpdatedAt: now}))

	err := store.Update(ctx, jobAccountA, "job-one", func(rec *JobRecord) {
		rec.State = JobStateReady
		rec.DocumentsTotal = 3
		rec.DocumentsDone = 2
		rec.FailedDocuments = []FailedDoc{{SourceKey: "bad.txt", Reason: "boom"}}
	})
	require.NoError(t, err)

	got, err := store.Get(ctx, jobAccountA, "job-one")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateReady, got.State)
	assert.Equal(t, 3, got.DocumentsTotal)
	assert.Equal(t, 2, got.DocumentsDone)
	require.Len(t, got.FailedDocuments, 1)
	assert.Equal(t, "bad.txt", got.FailedDocuments[0].SourceKey)
}

func TestJobStore_UpdateNotFound(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	err := store.Update(ctx, jobAccountA, "does-not-exist", func(rec *JobRecord) {})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrJobNotFound)
}

func TestJobStore_Delete(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, store.Reserve(ctx, jobAccountA, JobRecord{ID: "job-one", CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, store.Delete(ctx, jobAccountA, "job-one"))
	got, err := store.Get(ctx, jobAccountA, "job-one")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Deleting an already-absent record is a no-op success.
	require.NoError(t, store.Delete(ctx, jobAccountA, "job-one"))
}

// TestJobStore_PurgeAll proves PurgeAll clears every job record across every
// account, not just one -- the appliance-teardown use case it exists for.
func TestJobStore_PurgeAll(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, store.Reserve(ctx, jobAccountA, JobRecord{ID: "job-one", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.Reserve(ctx, jobAccountB, JobRecord{ID: "job-two", CreatedAt: now, UpdatedAt: now}))

	require.NoError(t, store.PurgeAll(ctx))

	all, err := store.ListAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, all)
}

// TestJobStore_PurgeAllOnEmptyBucketIsANoOp proves PurgeAll succeeds against
// a job store with no records yet -- a bucket kv.Keys reports as ErrNoKeysFound.
func TestJobStore_PurgeAllOnEmptyBucketIsANoOp(t *testing.T) {
	store := newJobStoreTestStore(t)
	require.NoError(t, store.PurgeAll(context.Background()))
}

func TestJobStore_ListAccountScoped(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, store.Reserve(ctx, jobAccountA, JobRecord{ID: "job-a1", IndexID: "idx-a", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.Reserve(ctx, jobAccountA, JobRecord{ID: "job-a2", IndexID: "idx-a", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.Reserve(ctx, jobAccountB, JobRecord{ID: "job-b1", IndexID: "idx-b", CreatedAt: now, UpdatedAt: now}))

	listA, err := store.List(ctx, jobAccountA)
	require.NoError(t, err)
	assert.Len(t, listA, 2)

	listB, err := store.List(ctx, jobAccountB)
	require.NoError(t, err)
	require.Len(t, listB, 1, "account B must not see account A's jobs")
	assert.Equal(t, "job-b1", listB[0].ID)
}

func TestJobStore_ListAll(t *testing.T) {
	store := newJobStoreTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, store.Reserve(ctx, jobAccountA, JobRecord{ID: "job-a1", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.Reserve(ctx, jobAccountB, JobRecord{ID: "job-b1", CreatedAt: now, UpdatedAt: now}))

	all, err := store.ListAll(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestJobStore_BucketRequiresJetStream(t *testing.T) {
	store := &JobStore{}
	_, err := store.bucket(context.Background())
	assert.Error(t, err)
}
