package handlers_ochrevector

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDSNEnv names the environment variable carrying a real Postgres/pgvector
// DSN. Unset in CI and by default locally, so this test skips cleanly rather
// than requiring a database (no testcontainers dependency is introduced).
const testDSNEnv = "OCHREVECTOR_TEST_DSN"

const (
	pgTestAccountA = "111111111111"
	pgTestAccountB = "222222222222"
)

// TestPgxBackend_Integration exercises pgxBackend against a real pgvector
// instance: Init/EnsureAccount/CreateIndex/DropIndex, plus the isolation
// invariant that one account's role cannot read another account's schema.
func TestPgxBackend_Integration(t *testing.T) {
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping pgvector integration test", testDSNEnv)
	}

	ctx := context.Background()
	backend, err := NewPgxBackend(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(backend.Close)

	require.NoError(t, backend.Init(ctx))
	require.NoError(t, backend.EnsureAccount(ctx, pgTestAccountA))
	require.NoError(t, backend.EnsureAccount(ctx, pgTestAccountB))

	const indexID = "idx-integrationtest01"
	t.Cleanup(func() {
		_ = backend.DropIndex(context.Background(), pgTestAccountA, indexID)
	})

	require.NoError(t, backend.CreateIndex(ctx, pgTestAccountA, IndexSpec{ID: indexID, Dimension: 8}))

	// Re-running CreateIndex must not error (IF NOT EXISTS), the property
	// Reconcile's forward-retry depends on.
	require.NoError(t, backend.CreateIndex(ctx, pgTestAccountA, IndexSpec{ID: indexID, Dimension: 8}))

	// Account B's role has no grant on account A's schema, so a query
	// against A's table under B's role must fail even though it is
	// syntactically well-formed.
	tx, err := backend.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL ROLE %s", sanitizeIdent(roleName(pgTestAccountB))))
	require.NoError(t, err)

	_, err = tx.Exec(ctx, fmt.Sprintf("SELECT 1 FROM %s.%s LIMIT 1",
		sanitizeIdent(schemaName(pgTestAccountA)), sanitizeIdent(tableName(indexID))))
	assert.Error(t, err, "account B's role must not be able to read account A's table")

	require.NoError(t, backend.DropIndex(ctx, pgTestAccountA, indexID))

	// Dropping an already-absent index is a no-op success.
	require.NoError(t, backend.DropIndex(ctx, pgTestAccountA, indexID))
}

// TestPgxBackend_ReplaceDocument_Integration proves ReplaceDocument's
// delete-then-reinsert replaces a source key's rows on re-run rather than
// accumulating duplicates (D7), against a real pgvector instance.
func TestPgxBackend_ReplaceDocument_Integration(t *testing.T) {
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping pgvector integration test", testDSNEnv)
	}

	ctx := context.Background()
	backend, err := NewPgxBackend(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(backend.Close)

	require.NoError(t, backend.Init(ctx))
	require.NoError(t, backend.EnsureAccount(ctx, pgTestAccountA))

	const indexID = "idx-replacedoctest01"
	t.Cleanup(func() {
		_ = backend.DropIndex(context.Background(), pgTestAccountA, indexID)
	})
	require.NoError(t, backend.CreateIndex(ctx, pgTestAccountA, IndexSpec{ID: indexID, Dimension: 4}))

	const sourceKey = "docs/one.txt"
	firstRows := []VectorRow{
		{Embedding: []float32{1, 0, 0, 0}, Chunk: "first chunk", Metadata: map[string]any{"k": "v"}, SourceOffset: 0},
		{Embedding: []float32{0, 1, 0, 0}, Chunk: "second chunk", SourceOffset: 100},
	}
	require.NoError(t, backend.ReplaceDocument(ctx, pgTestAccountA, indexID, sourceKey, firstRows))

	count, err := countDocumentRows(ctx, t, backend, pgTestAccountA, indexID, sourceKey)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// Re-ingesting the same key with a DIFFERENT chunk count must replace,
	// not accumulate: a stale query would otherwise see both the old and
	// new chunk sets at once.
	secondRows := []VectorRow{
		{Embedding: []float32{0, 0, 1, 0}, Chunk: "only chunk now", SourceOffset: 0},
	}
	require.NoError(t, backend.ReplaceDocument(ctx, pgTestAccountA, indexID, sourceKey, secondRows))

	count, err = countDocumentRows(ctx, t, backend, pgTestAccountA, indexID, sourceKey)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "ReplaceDocument must replace, not accumulate, rows for the same source key")

	// A different source key's rows are unaffected by another key's replace.
	require.NoError(t, backend.ReplaceDocument(ctx, pgTestAccountA, indexID, "docs/two.txt", []VectorRow{
		{Embedding: []float32{0, 0, 0, 1}, Chunk: "unrelated document", SourceOffset: 0},
	}))
	count, err = countDocumentRows(ctx, t, backend, pgTestAccountA, indexID, sourceKey)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// countDocumentRows counts sourceKey's rows in indexID's table under
// accountID's schema, using the account's own role (as query paths will).
func countDocumentRows(ctx context.Context, t *testing.T, backend *pgxBackend, accountID, indexID, sourceKey string) (int, error) {
	t.Helper()
	tx, err := backend.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL ROLE %s", sanitizeIdent(roleName(accountID))))
	require.NoError(t, err)
	_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL search_path = %s", sanitizeIdent(schemaName(accountID))))
	require.NoError(t, err)

	var count int
	err = tx.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE source_key = $1", sanitizeIdent(tableName(indexID))), sourceKey).Scan(&count)
	return count, err
}
