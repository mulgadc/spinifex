// In-package to match the sibling registry/jobs test files (registry_test.go,
// jobs_test.go) and exercise CAS/prefix-scan behavior directly.
//
//test:in-package
package handlers_ochrevector

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	kbAccountA = "111111111111"
	kbAccountB = "222222222222"
)

func newKBTestStore(t *testing.T) *KBStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return NewKBStore(js)
}

func newDataSourceTestStore(t *testing.T) *DataSourceStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return NewDataSourceStore(js)
}

func TestKBStore_CreateGetListDelete(t *testing.T) {
	store := newKBTestStore(t)
	ctx := context.Background()

	rec := KBRecord{ID: "kb-1", Name: "docs", Status: StateReady, Dimension: 1536}
	require.NoError(t, store.Create(ctx, kbAccountA, rec))

	got, err := store.Get(ctx, kbAccountA, "kb-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "docs", got.Name)
	assert.Equal(t, kbAccountA, got.AccountID)

	// A foreign account never sees another tenant's record.
	foreign, err := store.Get(ctx, kbAccountB, "kb-1")
	require.NoError(t, err)
	assert.Nil(t, foreign)

	list, err := store.List(ctx, kbAccountA)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	require.NoError(t, store.Delete(ctx, kbAccountA, "kb-1"))
	got, err = store.Get(ctx, kbAccountA, "kb-1")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Delete is idempotent: a second delete of an absent record still succeeds.
	require.NoError(t, store.Delete(ctx, kbAccountA, "kb-1"))
}

func TestKBStore_CreateCollisionReturnsErrKBExists(t *testing.T) {
	store := newKBTestStore(t)
	ctx := context.Background()

	rec := KBRecord{ID: "kb-1", Name: "docs", Status: StateReady}
	require.NoError(t, store.Create(ctx, kbAccountA, rec))

	err := store.Create(ctx, kbAccountA, rec)
	assert.ErrorIs(t, err, ErrKBExists)
}

func TestKBStore_ListScopedPerAccount(t *testing.T) {
	store := newKBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.Create(ctx, kbAccountA, KBRecord{ID: "kb-a1"}))
	require.NoError(t, store.Create(ctx, kbAccountA, KBRecord{ID: "kb-a2"}))
	require.NoError(t, store.Create(ctx, kbAccountB, KBRecord{ID: "kb-b1"}))

	listA, err := store.List(ctx, kbAccountA)
	require.NoError(t, err)
	assert.Len(t, listA, 2)

	listB, err := store.List(ctx, kbAccountB)
	require.NoError(t, err)
	assert.Len(t, listB, 1)
}

func TestDataSourceStore_CreateGetListDelete(t *testing.T) {
	store := newDataSourceTestStore(t)
	ctx := context.Background()

	rec := DataSourceRecord{ID: "ds-1", KnowledgeBaseID: "kb-1", Name: "s3-docs", Status: StateReady}
	require.NoError(t, store.Create(ctx, kbAccountA, rec))

	got, err := store.Get(ctx, kbAccountA, "ds-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "kb-1", got.KnowledgeBaseID)

	require.NoError(t, store.Delete(ctx, kbAccountA, "ds-1"))
	got, err = store.Get(ctx, kbAccountA, "ds-1")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Delete is idempotent.
	require.NoError(t, store.Delete(ctx, kbAccountA, "ds-1"))
}

func TestDataSourceStore_CreateCollisionReturnsErrDataSourceExists(t *testing.T) {
	store := newDataSourceTestStore(t)
	ctx := context.Background()

	rec := DataSourceRecord{ID: "ds-1", KnowledgeBaseID: "kb-1"}
	require.NoError(t, store.Create(ctx, kbAccountA, rec))

	err := store.Create(ctx, kbAccountA, rec)
	assert.ErrorIs(t, err, ErrDataSourceExists)
}

func TestDataSourceStore_ListByKnowledgeBase(t *testing.T) {
	store := newDataSourceTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.Create(ctx, kbAccountA, DataSourceRecord{ID: "ds-1", KnowledgeBaseID: "kb-1"}))
	require.NoError(t, store.Create(ctx, kbAccountA, DataSourceRecord{ID: "ds-2", KnowledgeBaseID: "kb-1"}))
	require.NoError(t, store.Create(ctx, kbAccountA, DataSourceRecord{ID: "ds-3", KnowledgeBaseID: "kb-2"}))

	scoped, err := store.ListByKnowledgeBase(ctx, kbAccountA, "kb-1")
	require.NoError(t, err)
	assert.Len(t, scoped, 2)

	scoped, err = store.ListByKnowledgeBase(ctx, kbAccountA, "kb-2")
	require.NoError(t, err)
	assert.Len(t, scoped, 1)

	scoped, err = store.ListByKnowledgeBase(ctx, kbAccountA, "kb-does-not-exist")
	require.NoError(t, err)
	assert.Empty(t, scoped)
}
