package handlers_ochrevector

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hnswM and hnswEfConstruction are the fixed HNSW build parameters (D10);
// ef_search stays runtime-tunable per query in a later stage.
const (
	hnswM              = 16
	hnswEfConstruction = 64
)

// pgxBackend is the pgx/v5 VectorBackend implementation. One pool is shared
// by every account (D3); it never carries a pool-wide search_path, since a
// shared pool serves every account and a stale connection-level setting
// would leak across them. Every account-scoped operation instead opens a
// transaction and sets ROLE and search_path LOCAL to that transaction alone.
type pgxBackend struct {
	pool *pgxpool.Pool
}

var _ VectorBackend = (*pgxBackend)(nil)

// NewPgxBackend connects a pooled pgx/v5 client to dsn. The pool is shared
// across every account; per-account isolation is enforced per-operation, not
// at the pool level (see pgxBackend).
func NewPgxBackend(ctx context.Context, dsn string) (*pgxBackend, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: connect postgres: %w", err)
	}
	return &pgxBackend{pool: pool}, nil
}

// Close releases the pool's connections. Safe to call once, at shutdown.
func (b *pgxBackend) Close() {
	b.pool.Close()
}

// Init creates the vector extension if it does not already exist. The
// extension is database-scoped, so this runs once at daemon boot rather than
// per account.
func (b *pgxBackend) Init(ctx context.Context) error {
	if _, err := b.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("ochrevector: create vector extension: %w", err)
	}
	return nil
}

// EnsureAccount creates accountID's schema and a non-login role granted
// USAGE and CREATE on it alone, with no access to any other schema. Safe to
// call before every account-scoped operation: every statement here is
// idempotent (CREATE ... IF NOT EXISTS, or a pre-check before CREATE ROLE,
// which has no IF NOT EXISTS form).
func (b *pgxBackend) EnsureAccount(ctx context.Context, accountID string) error {
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	schema := sanitizeIdent(schemaName(accountID))
	role := sanitizeIdent(roleName(accountID))

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ochrevector: begin ensure-account tx for %s: %w", accountID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// #nosec G201 -- schema is an identifier sanitized by pgx.Identifier and
	// validated against indexIDPattern-equivalent account-id validation above;
	// no user value is ever concatenated here.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema)); err != nil {
		return fmt.Errorf("ochrevector: create schema for account %s: %w", accountID, err)
	}

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, roleName(accountID)).Scan(&exists); err != nil {
		return fmt.Errorf("ochrevector: check role for account %s: %w", accountID, err)
	}
	if !exists {
		// #nosec G201 -- role is a sanitized, validated identifier; CREATE ROLE
		// has no parameterized form and no IF NOT EXISTS, hence the pre-check.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, role)); err != nil {
			return fmt.Errorf("ochrevector: create role for account %s: %w", accountID, err)
		}
	}

	// #nosec G201 -- schema/role are sanitized, validated identifiers.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA %s TO %s`, schema, role)); err != nil {
		return fmt.Errorf("ochrevector: grant schema to role for account %s: %w", accountID, err)
	}
	// The account role never gets USAGE on any other schema (including
	// public), so cross-account access requires an explicit grant this code
	// never makes — isolation holds even against a query bug elsewhere.
	// #nosec G201 -- role is a sanitized, validated identifier.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`REVOKE ALL ON SCHEMA public FROM %s`, role)); err != nil {
		return fmt.Errorf("ochrevector: revoke public schema from role for account %s: %w", accountID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ochrevector: commit ensure-account for account %s: %w", accountID, err)
	}
	return nil
}

// withAccountTx runs fn inside a transaction scoped to accountID: SET LOCAL
// ROLE to the account's role and SET LOCAL search_path to its schema, so
// every statement fn issues is enforced by Postgres' own grants — even a
// query bug in fn cannot reach another account's schema, because the role
// itself has no grant there.
func (b *pgxBackend) withAccountTx(ctx context.Context, accountID string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ochrevector: begin account tx for %s: %w", accountID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	role := sanitizeIdent(roleName(accountID))
	schema := sanitizeIdent(schemaName(accountID))
	// #nosec G201 -- role is a sanitized, validated identifier; SET does not
	// accept bound parameters.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL ROLE %s`, role)); err != nil {
		return fmt.Errorf("ochrevector: set local role for account %s: %w", accountID, err)
	}
	// #nosec G201 -- schema is a sanitized, validated identifier; SET does not
	// accept bound parameters.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL search_path = %s`, schema)); err != nil {
		return fmt.Errorf("ochrevector: set local search_path for account %s: %w", accountID, err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ochrevector: commit account tx for account %s: %w", accountID, err)
	}
	return nil
}

// CreateIndex creates spec's backing table and HNSW index under accountID's
// schema. Both statements are IF NOT EXISTS, so a retry after a crash
// mid-create (the Reconcile path) is safe rather than erroring on a
// partially-created index.
func (b *pgxBackend) CreateIndex(ctx context.Context, accountID string, spec IndexSpec) error {
	if err := validateIndexID(spec.ID); err != nil {
		return err
	}
	if spec.Dimension <= 0 {
		return fmt.Errorf("ochrevector: index %s: dimension must be positive, got %d", spec.ID, spec.Dimension)
	}
	table := sanitizeIdent(tableName(spec.ID))
	hnswIdx := sanitizeIdent(hnswIndexName(spec.ID))

	return b.withAccountTx(ctx, accountID, func(ctx context.Context, tx pgx.Tx) error {
		// #nosec G201 -- table is a sanitized, validated identifier; Dimension
		// is a validated positive int, not a caller string — vector(N) has no
		// parameterized form.
		createTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id bigserial PRIMARY KEY,
			embedding vector(%d) NOT NULL,
			chunk text NOT NULL,
			metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
			source_key text NOT NULL,
			source_offset integer NOT NULL DEFAULT 0
		)`, table, spec.Dimension)
		if _, err := tx.Exec(ctx, createTable); err != nil {
			return fmt.Errorf("ochrevector: create table for index %s: %w", spec.ID, err)
		}

		// #nosec G201 -- hnswIdx/table are sanitized, validated identifiers.
		createIndex := fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (embedding vector_cosine_ops) WITH (m = %d, ef_construction = %d)`,
			hnswIdx, table, hnswM, hnswEfConstruction)
		if _, err := tx.Exec(ctx, createIndex); err != nil {
			return fmt.Errorf("ochrevector: create hnsw index for index %s: %w", spec.ID, err)
		}
		return nil
	})
}

// DropIndex drops indexID's backing table under accountID's schema. Idempotent:
// dropping an already-absent index is a no-op success.
func (b *pgxBackend) DropIndex(ctx context.Context, accountID, indexID string) error {
	if err := validateIndexID(indexID); err != nil {
		return err
	}
	table := sanitizeIdent(tableName(indexID))

	return b.withAccountTx(ctx, accountID, func(ctx context.Context, tx pgx.Tx) error {
		// #nosec G201 -- table is a sanitized, validated identifier.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)); err != nil {
			return fmt.Errorf("ochrevector: drop table for index %s: %w", indexID, err)
		}
		return nil
	})
}

// ReplaceDocument deletes every row for sourceKey then reinserts rows, in one
// transaction (D7): a query mid-ingest never sees a half-replaced document,
// and a re-ingest of the same key replaces rather than accumulates.
func (b *pgxBackend) ReplaceDocument(ctx context.Context, accountID, indexID, sourceKey string, rows []VectorRow) error {
	if err := validateIndexID(indexID); err != nil {
		return err
	}
	table := sanitizeIdent(tableName(indexID))

	return b.withAccountTx(ctx, accountID, func(ctx context.Context, tx pgx.Tx) error {
		// #nosec G201 -- table is a sanitized, validated identifier; sourceKey
		// is bound as a parameter.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE source_key = $1`, table), sourceKey); err != nil {
			return fmt.Errorf("ochrevector: delete existing rows for %s: %w", sourceKey, err)
		}

		// #nosec G201 -- table is a sanitized, validated identifier; every
		// value below is bound as a parameter. The embedding has no pgx
		// native type here, so it is bound as pgvector's own "[v1,v2,...]"
		// text form and cast server-side via ::vector.
		insert := fmt.Sprintf(`INSERT INTO %s (embedding, chunk, metadata, source_key, source_offset) VALUES ($1::vector, $2, $3::jsonb, $4, $5)`, table)
		for _, row := range rows {
			metadata := row.Metadata
			if metadata == nil {
				metadata = map[string]any{}
			}
			metaJSON, err := json.Marshal(metadata)
			if err != nil {
				return fmt.Errorf("ochrevector: encode metadata for %s: %w", sourceKey, err)
			}
			if _, err := tx.Exec(ctx, insert, encodeVector(row.Embedding), row.Chunk, metaJSON, sourceKey, row.SourceOffset); err != nil {
				return fmt.Errorf("ochrevector: insert row for %s: %w", sourceKey, err)
			}
		}
		return nil
	})
}

// encodeVector renders embedding in pgvector's own text input format
// ("[v1,v2,...]"). Bound as an ordinary string parameter and cast
// server-side via ::vector, so no pgvector client dependency is needed for
// this one value encoding.
func encodeVector(embedding []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, v := range embedding {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}
