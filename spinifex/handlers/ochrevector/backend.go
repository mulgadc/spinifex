// Package handlers_ochrevector holds Ochre's tenant vector store: the
// pgvector backend, the KV index registry, and the create/delete/list/
// reconcile orchestration over both.
package handlers_ochrevector

import "context"

// IndexSpec describes a single vector index at creation time. Dimension is
// fixed for the index's lifetime, stamped from the embedding model that
// produced it.
type IndexSpec struct {
	ID        string
	Dimension int
}

// VectorRow is one chunk's embedding plus its provenance, ready for
// ReplaceDocument to persist. Metadata is marshalled to jsonb as-is; a nil
// map persists as an empty jsonb object rather than SQL NULL.
type VectorRow struct {
	Embedding    []float32
	Chunk        string
	Metadata     map[string]any
	SourceOffset int
}

// VectorBackend is the swap-out seam behind every Postgres/pgvector
// operation: a future HA or replicated backend implements this interface
// without any caller change. Only the operations this stage needs are
// declared here; ingestion and query are later stages' extensions.
type VectorBackend interface {
	// Init runs once-per-database setup (CREATE EXTENSION vector). Idempotent:
	// safe to call on every daemon boot.
	Init(ctx context.Context) error

	// EnsureAccount provisions accountID's schema, role and grants if they do
	// not already exist. Idempotent: safe to call before every account-scoped
	// operation.
	EnsureAccount(ctx context.Context, accountID string) error

	// CreateIndex creates the backing table and HNSW index for spec under
	// accountID's schema. Idempotent: safe to retry after a crash mid-create.
	CreateIndex(ctx context.Context, accountID string, spec IndexSpec) error

	// DropIndex drops indexID's backing table under accountID's schema.
	// Idempotent: dropping an already-absent index is a no-op success.
	DropIndex(ctx context.Context, accountID, indexID string) error

	// ReplaceDocument atomically replaces every row previously stored for
	// sourceKey under indexID with rows, in one transaction: delete-by-key
	// then reinsert. A query never observes a half-replaced document, and a
	// re-ingest of the same source key replaces rather than duplicates its
	// chunks (D7). An empty rows deletes the key's existing rows and inserts
	// nothing.
	ReplaceDocument(ctx context.Context, accountID, indexID, sourceKey string, rows []VectorRow) error
}
