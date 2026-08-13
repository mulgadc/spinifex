// Exercises the unexported service orchestration internals with no
// exported surface to drive them through.
//
//test:in-package
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
	svcAccountA = "111111111111"
	svcAccountB = "222222222222"
)

func newServiceTestSetup(t *testing.T) (*Service, *fakeBackend) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	backend := newFakeBackend()
	return NewService(NewRegistry(js), backend), backend
}

func TestService_CreateIndex_HappyPath(t *testing.T) {
	svc, backend := newServiceTestSetup(t)
	ctx := context.Background()

	rec, err := svc.CreateIndex(ctx, svcAccountA, "idx-one", CreateIndexParams{Name: "kb1", Dimension: 768, EmbeddingModel: "nomic-embed-text-v1.5"})
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, StateReady, rec.State)
	assert.Equal(t, svcAccountA, rec.AccountID)

	assert.True(t, backend.hasAccount(svcAccountA))
	assert.True(t, backend.hasIndex(svcAccountA, "idx-one"))

	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, StateReady, stored.State)
}

func TestService_CreateIndex_ReserveCollision(t *testing.T) {
	svc, _ := newServiceTestSetup(t)
	ctx := context.Background()
	params := CreateIndexParams{Name: "kb1", Dimension: 768}

	_, err := svc.CreateIndex(ctx, svcAccountA, "idx-one", params)
	require.NoError(t, err)

	_, err = svc.CreateIndex(ctx, svcAccountA, "idx-one", params)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexExists)

	// The first index survives the second (failed) create attempt untouched.
	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, StateReady, stored.State)
}

func TestService_CreateIndex_RollsBackOnBackendFailure(t *testing.T) {
	svc, backend := newServiceTestSetup(t)
	ctx := context.Background()
	backend.failCreateIndex = errFakeBackend

	_, err := svc.CreateIndex(ctx, svcAccountA, "idx-one", CreateIndexParams{Name: "kb1", Dimension: 768})
	require.Error(t, err)

	// No half-state: no registry record and no backend index survive the
	// failed create.
	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-one")
	require.NoError(t, err)
	assert.Nil(t, stored, "a failed create must not leave a registry record behind")
	assert.False(t, backend.hasIndex(svcAccountA, "idx-one"))
}

func TestService_CreateIndex_RollsBackOnEnsureAccountFailure(t *testing.T) {
	svc, backend := newServiceTestSetup(t)
	ctx := context.Background()
	backend.failEnsureAccount = errFakeBackend

	_, err := svc.CreateIndex(ctx, svcAccountA, "idx-one", CreateIndexParams{Name: "kb1", Dimension: 768})
	require.Error(t, err)

	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-one")
	require.NoError(t, err)
	assert.Nil(t, stored)
}

func TestService_CreateIndex_RejectsNonPositiveDimension(t *testing.T) {
	svc, _ := newServiceTestSetup(t)
	ctx := context.Background()

	_, err := svc.CreateIndex(ctx, svcAccountA, "idx-one", CreateIndexParams{Name: "kb1", Dimension: 0})
	require.Error(t, err)

	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-one")
	require.NoError(t, err)
	assert.Nil(t, stored, "an invalid create must never reach Reserve")
}

func TestService_DeleteIndex_HappyPath(t *testing.T) {
	svc, backend := newServiceTestSetup(t)
	ctx := context.Background()

	_, err := svc.CreateIndex(ctx, svcAccountA, "idx-one", CreateIndexParams{Name: "kb1", Dimension: 768})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteIndex(ctx, svcAccountA, "idx-one"))

	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-one")
	require.NoError(t, err)
	assert.Nil(t, stored)
	assert.False(t, backend.hasIndex(svcAccountA, "idx-one"))
}

func TestService_DeleteIndex_AbsentIsNoop(t *testing.T) {
	svc, _ := newServiceTestSetup(t)
	ctx := context.Background()

	require.NoError(t, svc.DeleteIndex(ctx, svcAccountA, "does-not-exist"))
}

func TestService_ListIndexes_AccountScoped(t *testing.T) {
	svc, _ := newServiceTestSetup(t)
	ctx := context.Background()

	_, err := svc.CreateIndex(ctx, svcAccountA, "idx-a1", CreateIndexParams{Name: "a1", Dimension: 768})
	require.NoError(t, err)
	_, err = svc.CreateIndex(ctx, svcAccountB, "idx-b1", CreateIndexParams{Name: "b1", Dimension: 768})
	require.NoError(t, err)

	listA, err := svc.ListIndexes(ctx, svcAccountA)
	require.NoError(t, err)
	require.Len(t, listA, 1)
	assert.Equal(t, "idx-a1", listA[0].ID)

	listB, err := svc.ListIndexes(ctx, svcAccountB)
	require.NoError(t, err)
	require.Len(t, listB, 1, "account B must not see account A's indexes")
	assert.Equal(t, "idx-b1", listB[0].ID)
}

// seedStuckRecord reserves a record directly (bypassing CreateIndex/
// DeleteIndex) with an UpdatedAt old enough for Reconcile to treat it as
// abandoned by a crashed writer.
func seedStuckRecord(t *testing.T, svc *Service, accountID, indexID, state string) {
	t.Helper()
	stuckAt := time.Now().UTC().Add(-2 * reconcileStuckAfter)
	rec := Record{
		ID:        indexID,
		Name:      "stuck",
		Dimension: 768,
		State:     state,
		CreatedAt: stuckAt,
		UpdatedAt: stuckAt,
	}
	require.NoError(t, svc.Registry.Reserve(context.Background(), accountID, rec))
}

func TestService_Reconcile_FinishesStuckCreating(t *testing.T) {
	svc, backend := newServiceTestSetup(t)
	ctx := context.Background()

	// Simulates a crash after Reserve but before the backend ever ran.
	seedStuckRecord(t, svc, svcAccountA, "idx-stuck-create", StateCreating)
	assert.False(t, backend.hasIndex(svcAccountA, "idx-stuck-create"))

	require.NoError(t, svc.Reconcile(ctx))

	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-stuck-create")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, StateReady, stored.State)
	assert.True(t, backend.hasIndex(svcAccountA, "idx-stuck-create"))
}

func TestService_Reconcile_FinishesStuckDeleting(t *testing.T) {
	svc, backend := newServiceTestSetup(t)
	ctx := context.Background()

	// Simulates a crash after SetState(DELETING) but before DropIndex ran:
	// the backend still has the table.
	seedStuckRecord(t, svc, svcAccountA, "idx-stuck-delete", StateDeleting)
	require.NoError(t, backend.CreateIndex(ctx, svcAccountA, IndexSpec{ID: "idx-stuck-delete", Dimension: 768}))
	assert.True(t, backend.hasIndex(svcAccountA, "idx-stuck-delete"))

	require.NoError(t, svc.Reconcile(ctx))

	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-stuck-delete")
	require.NoError(t, err)
	assert.Nil(t, stored)
	assert.False(t, backend.hasIndex(svcAccountA, "idx-stuck-delete"))
}

func TestService_Reconcile_LeavesFreshRecordsAlone(t *testing.T) {
	svc, backend := newServiceTestSetup(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, svc.Registry.Reserve(ctx, svcAccountA, Record{
		ID: "idx-fresh", Dimension: 768, State: StateCreating, CreatedAt: now, UpdatedAt: now,
	}))

	require.NoError(t, svc.Reconcile(ctx))

	stored, err := svc.Registry.Get(ctx, svcAccountA, "idx-fresh")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, StateCreating, stored.State, "a record still within the grace period must not be disturbed")
	assert.False(t, backend.hasIndex(svcAccountA, "idx-fresh"))
}
