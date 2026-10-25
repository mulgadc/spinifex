// Exercises the unexported pgx backend internals with no exported
// surface to drive them through.
//
//test:in-package
package vector

import (
	"context"
	"fmt"
	"os"
	"strings"
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

	exists, err := backend.IndexExists(ctx, pgTestAccountA, indexID)
	require.NoError(t, err)
	assert.False(t, exists, "index must not exist before CreateIndex")

	require.NoError(t, backend.CreateIndex(ctx, pgTestAccountA, IndexSpec{ID: indexID, Dimension: 8}))

	exists, err = backend.IndexExists(ctx, pgTestAccountA, indexID)
	require.NoError(t, err)
	assert.True(t, exists, "index must exist after CreateIndex")

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

	exists, err = backend.IndexExists(ctx, pgTestAccountA, indexID)
	require.NoError(t, err)
	assert.False(t, exists, "index must not exist after DropIndex")

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

// TestPgxBackend_Query_Integration proves Query against a real pgvector
// instance: nearest-first ordering, a score in [0,1] (D8), and a metadata
// filter (D9) narrowing the result set correctly.
func TestPgxBackend_Query_Integration(t *testing.T) {
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

	const indexID = "idx-querytest01"
	t.Cleanup(func() {
		_ = backend.DropIndex(context.Background(), pgTestAccountA, indexID)
	})
	require.NoError(t, backend.CreateIndex(ctx, pgTestAccountA, IndexSpec{ID: indexID, Dimension: 3}))

	rows := []struct {
		key string
		row VectorRow
	}{
		{"docs/near.txt", VectorRow{Embedding: []float32{1, 0, 0}, Chunk: "closest to the query", Metadata: map[string]any{"category": "handbook"}}},
		{"docs/mid.txt", VectorRow{Embedding: []float32{0.7, 0.7, 0}, Chunk: "somewhat related", Metadata: map[string]any{"category": "faq"}}},
		{"docs/far.txt", VectorRow{Embedding: []float32{0, 0, 1}, Chunk: "unrelated", Metadata: map[string]any{"category": "faq"}}},
	}
	for _, r := range rows {
		require.NoError(t, backend.ReplaceDocument(ctx, pgTestAccountA, indexID, r.key, []VectorRow{r.row}))
	}

	// Unfiltered: nearest first, every score in [0,1].
	results, err := backend.Query(ctx, pgTestAccountA, indexID, []float32{1, 0, 0}, 3, nil)
	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.Equal(t, "docs/near.txt", results[0].SourceKey, "the identical vector must rank first")
	for _, res := range results {
		assert.GreaterOrEqual(t, res.Score, float32(0))
		assert.LessOrEqual(t, res.Score, float32(1))
	}
	assert.GreaterOrEqual(t, results[0].Score, results[1].Score, "results must be nearest-first")
	assert.GreaterOrEqual(t, results[1].Score, results[2].Score, "results must be nearest-first")

	// Filtered: category=faq narrows to the two faq-tagged rows only.
	filtered, err := backend.Query(ctx, pgTestAccountA, indexID, []float32{1, 0, 0}, 10, Equals("category", "faq"))
	require.NoError(t, err)
	require.Len(t, filtered, 2)
	for _, res := range filtered {
		assert.Equal(t, "faq", res.Metadata["category"])
	}
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

// TestPgxBackend_EnsureAccount_GrantFailure_Integration fails EnsureAccount's
// schema grant after CREATE SCHEMA and CREATE ROLE have run, and proves the
// schema is rolled back with it rather than left behind ungranted.
func TestPgxBackend_EnsureAccount_GrantFailure_Integration(t *testing.T) {
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping pgvector integration test", testDSNEnv)
	}

	ctx := context.Background()
	backend, err := NewPgxBackend(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(backend.Close)

	const accountID = "333333333333"
	dropAccount(ctx, t, backend, accountID)
	t.Cleanup(func() { dropAccount(context.Background(), t, backend, accountID) })

	removeTrigger := failGrantsMatching(ctx, t, backend, "%ON SCHEMA%")
	err = backend.EnsureAccount(ctx, accountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "grant schema to role")

	var schemaExists, roleExists bool
	require.NoError(t, backend.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schemaName(accountID)).Scan(&schemaExists))
	require.NoError(t, backend.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, roleName(accountID)).Scan(&roleExists))
	assert.False(t, schemaExists, "a failed grant must roll back the schema it would have granted")
	assert.False(t, roleExists, "a failed grant must roll back the role it would have granted to")

	removeTrigger()
	require.NoError(t, backend.EnsureAccount(ctx, accountID))

	var usage, create bool
	require.NoError(t, backend.pool.QueryRow(ctx,
		`SELECT has_schema_privilege($1, $2, 'USAGE'), has_schema_privilege($1, $2, 'CREATE')`,
		roleName(accountID), schemaName(accountID)).Scan(&usage, &create))
	assert.True(t, usage, "a retried EnsureAccount must grant USAGE on the schema")
	assert.True(t, create, "a retried EnsureAccount must grant CREATE on the schema")
}

// TestPgxBackend_RegrantAccount_PartialFailure_Integration fails
// RegrantAccount's sequence grant after its table grant has run, as a restore
// would hit it, and proves a retry leaves the role able to insert again.
func TestPgxBackend_RegrantAccount_PartialFailure_Integration(t *testing.T) {
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping pgvector integration test", testDSNEnv)
	}

	ctx := context.Background()
	backend, err := NewPgxBackend(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(backend.Close)

	const accountID = "444444444444"
	dropAccount(ctx, t, backend, accountID)
	t.Cleanup(func() { dropAccount(context.Background(), t, backend, accountID) })
	require.NoError(t, backend.EnsureAccount(ctx, accountID))

	// A restore recreates the account's tables owned by the connecting role,
	// not the account role, so the account role starts with no grant on them.
	table := sanitizeIdent(schemaName(accountID)) + "." + sanitizeIdent("restored")
	_, err = backend.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id bigserial PRIMARY KEY, chunk text NOT NULL)`, table))
	require.NoError(t, err)

	removeTrigger := failGrantsMatching(ctx, t, backend, "%ALL SEQUENCES%")
	err = backend.RegrantAccount(ctx, accountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "regrant sequences")

	removeTrigger()
	require.NoError(t, backend.RegrantAccount(ctx, accountID))

	tx, err := backend.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL ROLE %s", sanitizeIdent(roleName(accountID))))
	require.NoError(t, err)
	_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (chunk) VALUES ('after retry')`, table))
	assert.NoError(t, err, "a retried RegrantAccount must restore insert access, sequence included")
}

// failGrantsMatching installs a database-wide event trigger that aborts any
// GRANT whose statement text matches the LIKE pattern, so a test can fail one
// step of a multi-statement grant. The returned func removes it early.
func failGrantsMatching(ctx context.Context, t *testing.T, backend *pgxBackend, pattern string) func() {
	t.Helper()
	_, err := backend.pool.Exec(ctx, fmt.Sprintf(`CREATE OR REPLACE FUNCTION ochrevector_test_fail_grant() RETURNS event_trigger LANGUAGE plpgsql AS $$
BEGIN
	IF current_query() LIKE %s THEN
		RAISE EXCEPTION 'injected grant failure';
	END IF;
END $$`, quoteLiteral(pattern)))
	require.NoError(t, err)
	_, err = backend.pool.Exec(ctx, `CREATE EVENT TRIGGER ochrevector_test_fail_grant ON ddl_command_start WHEN TAG IN ('GRANT') EXECUTE FUNCTION ochrevector_test_fail_grant()`)
	require.NoError(t, err)

	remove := func() {
		_, err := backend.pool.Exec(context.Background(), `DROP EVENT TRIGGER IF EXISTS ochrevector_test_fail_grant`)
		require.NoError(t, err)
		_, err = backend.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS ochrevector_test_fail_grant()`)
		require.NoError(t, err)
	}
	t.Cleanup(remove)
	return remove
}

// quoteLiteral renders s as a single-quoted SQL string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// dropAccount removes accountID's schema and role so a test starts from, and
// leaves behind, an account that was never ensured.
func dropAccount(ctx context.Context, t *testing.T, backend *pgxBackend, accountID string) {
	t.Helper()
	_, err := backend.pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, sanitizeIdent(schemaName(accountID))))
	require.NoError(t, err)
	_, err = backend.pool.Exec(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, sanitizeIdent(roleName(accountID))))
	require.NoError(t, err)
}
