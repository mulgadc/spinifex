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
