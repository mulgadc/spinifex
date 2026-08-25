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

// extensionsSchema holds pgvector, out of public and out of every account
// schema. Account roles get USAGE (read-only) on it and carry it on their
// search_path so the vector type and its opclasses resolve unqualified.
const extensionsSchema = "extensions"

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

// Init ensures the vector extension exists in the extensions schema. Both are
// database-scoped, so this runs once at daemon boot. In production the appliance
// bootstrap has already installed them as the superuser, so these IF-NOT-EXISTS
// statements are a no-op; against a superuser test DSN they create them.
func (b *pgxBackend) Init(ctx context.Context) error {
	if _, err := b.pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, extensionsSchema)); err != nil {
		return fmt.Errorf("ochrevector: create extensions schema: %w", err)
	}
	if _, err := b.pool.Exec(ctx, fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS vector SCHEMA %s`, extensionsSchema)); err != nil {
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

// RegrantAccount grants the account role full privileges on every table and
// sequence currently in its schema. A restore imports a dump taken with
// --no-owner/--no-privileges as the master role, which recreates each object
// owned by the master role rather than the account role; this closes that
// gap so query/ingest access is unaffected by which role ran the restore.
func (b *pgxBackend) RegrantAccount(ctx context.Context, accountID string) error {
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	schema := sanitizeIdent(schemaName(accountID))
	role := sanitizeIdent(roleName(accountID))

	// #nosec G201 -- schema/role are sanitized, validated identifiers; GRANT
	// does not accept bound parameters.
	if _, err := b.pool.Exec(ctx, fmt.Sprintf(`GRANT ALL ON ALL TABLES IN SCHEMA %s TO %s`, schema, role)); err != nil {
		return fmt.Errorf("ochrevector: regrant tables for account %s: %w", accountID, err)
	}
	// #nosec G201 -- schema/role are sanitized, validated identifiers.
	if _, err := b.pool.Exec(ctx, fmt.Sprintf(`GRANT ALL ON ALL SEQUENCES IN SCHEMA %s TO %s`, schema, role)); err != nil {
		return fmt.Errorf("ochrevector: regrant sequences for account %s: %w", accountID, err)
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
	// #nosec G201 -- schema is a sanitized, validated identifier and
	// extensionsSchema is a fixed constant; SET does not accept bound parameters.
	// extensions trails the account schema so the account's own objects win name
	// resolution and only the shared vector type falls through to it.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL search_path = %s, %s`, schema, extensionsSchema)); err != nil {
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

// IndexExists reports whether indexID's backing table exists under
// accountID's schema. to_regclass resolves to NULL for a schema or table
// that is not there rather than erroring, so no account role or search_path
// setup is needed for this read-only lookup.
func (b *pgxBackend) IndexExists(ctx context.Context, accountID, indexID string) (bool, error) {
	if err := validateAccountID(accountID); err != nil {
		return false, err
	}
	if err := validateIndexID(indexID); err != nil {
		return false, err
	}
	qualified := schemaName(accountID) + "." + tableName(indexID)
	var regclass *string
	if err := b.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, qualified).Scan(&regclass); err != nil {
		return false, fmt.Errorf("ochrevector: check index %s exists for account %s: %w", indexID, accountID, err)
	}
	return regclass != nil, nil
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

// Query runs a k-nearest-neighbour cosine similarity search against
// indexID's embedding column under accountID's schema (D8), optionally
// narrowed by filter (D9). The query vector is always bound param $1; a
// non-nil filter's own bound params continue contiguously from $2, and the
// LIMIT is the last param, so numbering matches the compiled WHERE clause
// exactly regardless of whether a filter is present.
func (b *pgxBackend) Query(ctx context.Context, accountID, indexID string, queryVector []float32, k int, filter *Filter) ([]QueryResult, error) {
	if err := validateIndexID(indexID); err != nil {
		return nil, err
	}
	if len(queryVector) == 0 {
		return nil, fmt.Errorf("ochrevector: query %s: empty query vector", indexID)
	}
	table := sanitizeIdent(tableName(indexID))

	args := []any{encodeVector(queryVector)}
	whereSQL := ""
	limitParam := 2
	if filter != nil {
		sql, filterArgs, next, err := filter.compile(2)
		if err != nil {
			return nil, fmt.Errorf("ochrevector: query %s: compile filter: %w", indexID, err)
		}
		whereSQL = " WHERE " + sql
		args = append(args, filterArgs...)
		limitParam = next
	}
	args = append(args, clampQueryK(k))

	// #nosec G201 -- table is a sanitized, validated identifier; whereSQL
	// comes only from filter.compile, whose metadata keys pass an
	// identifier allowlist before ever reaching SQL text and whose values
	// are always appended to args as bound params, never interpolated here.
	query := fmt.Sprintf(
		`SELECT chunk, metadata, source_key, source_offset, 1 - (embedding <=> $1::vector) AS score FROM %s%s ORDER BY embedding <=> $1::vector LIMIT $%d`,
		table, whereSQL, limitParam)

	var results []QueryResult
	err := b.withAccountTx(ctx, accountID, func(ctx context.Context, tx pgx.Tx) error {
		if filter != nil {
			// A selective filter under HNSW needs iterative scan to still
			// surface k results rather than stopping short on the index's
			// default candidate list (D9); with no filter the default
			// (non-iterative) scan is left in place.
			if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
				return fmt.Errorf("set iterative scan: %w", err)
			}
		}

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("query: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				res      QueryResult
				metaJSON []byte
			)
			if err := rows.Scan(&res.Chunk, &metaJSON, &res.SourceKey, &res.SourceOffset, &res.Score); err != nil {
				return fmt.Errorf("scan row: %w", err)
			}
			if len(metaJSON) > 0 {
				if err := json.Unmarshal(metaJSON, &res.Metadata); err != nil {
					return fmt.Errorf("decode metadata: %w", err)
				}
			}
			results = append(results, res)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("ochrevector: query %s: %w", indexID, err)
	}
	return results, nil
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
