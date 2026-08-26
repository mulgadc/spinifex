package handlers_ochrevector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// registryBucket is the cluster-replicated KV bucket holding index registry
// records (D4): the authoritative source for an index's existence,
// configuration and state, surviving a Postgres rebuild even though the
// vectors themselves do not.
const registryBucket = "ochre-vector-indexes"

// registryBucketHistory keeps one revision per key: a registry record is one
// JSON document mutated in place, not a series.
const registryBucketHistory = 1

// Index lifecycle states (D4).
const (
	StateCreating = "CREATING"
	StateReady    = "READY"
	StateDeleting = "DELETING"
	StateStale    = "STALE"
)

// ErrIndexExists reports that Reserve lost the single-writer claim on an
// index id already reserved for that account.
var ErrIndexExists = errors.New("ochrevector: index already exists")

// ErrIndexNotFound reports that an operation targeted an account+index pair
// with no registry record.
var ErrIndexNotFound = errors.New("ochrevector: index not found")

// SourceSpec is one ingestion source bound to an index: where its documents
// come from and how they are chunked/embedded. Populated by the ingestion
// stage; persisted here so a rebuilt index can re-ingest without operator
// input (D4).
type SourceSpec struct {
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	ChunkSize      int    `json:"chunkSize"`
	ChunkOverlap   int    `json:"chunkOverlap"`
	EmbeddingModel string `json:"embeddingModel"`
	Dimension      int    `json:"dimension"`
	// Metadata is a static set of per-source tags stamped onto every row
	// ingested from this source (see ingestObject), giving D9 filters
	// something to match against beyond the source key.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// sourceSpecEqual reports whether a and b are the same source spec.
// SourceSpec carries a map (Metadata), so it is not comparable with ==;
// AppendSourceSpec's dedup check uses this instead of slices.Contains.
func sourceSpecEqual(a, b SourceSpec) bool {
	return a.Bucket == b.Bucket &&
		a.Prefix == b.Prefix &&
		a.ChunkSize == b.ChunkSize &&
		a.ChunkOverlap == b.ChunkOverlap &&
		a.EmbeddingModel == b.EmbeddingModel &&
		a.Dimension == b.Dimension &&
		maps.Equal(a.Metadata, b.Metadata)
}

// Record is the authoritative registry entry for one vector index (D4).
type Record struct {
	ID             string       `json:"id"`
	AccountID      string       `json:"accountId"`
	Name           string       `json:"name"`
	Dimension      int          `json:"dimension"`
	EmbeddingModel string       `json:"embeddingModel"`
	State          string       `json:"state"`
	SourceSpecs    []SourceSpec `json:"sourceSpecs,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
}

// Registry persists index Records in the ochre-vector-indexes JetStream KV
// bucket, keyed "accountID/id" with a per-account prefix-scan list.
type Registry struct {
	store *kvstore.Store[Record]
}

// NewRegistry constructs a Registry over js.
func NewRegistry(js jetstream.JetStream) *Registry {
	return &Registry{store: kvstore.New[Record](js, kvstore.Config{
		Name:    registryBucket,
		History: registryBucketHistory,
		Missing: "ochrevector: registry has no JetStream client configured",
	})}
}

// registryKey scopes every record to its owning account, so a foreign
// account's raw-id guess can never collide with another tenant's key.
func registryKey(accountID, indexID string) string {
	return accountID + "/" + indexID
}

// Reserve atomically claims rec.ID for accountID: the create-only KV write is
// the single-writer mutex (mirrors handlers/rds's identifier claim), so two
// concurrent creates of the same id race safely and exactly one wins.
// rec.AccountID is stamped from accountID, overriding whatever the caller set.
func (reg *Registry) Reserve(ctx context.Context, accountID string, rec Record) error {
	rec.AccountID = accountID
	if err := reg.store.Create(ctx, registryKey(accountID, rec.ID), &rec); err != nil {
		if errors.Is(err, kvstore.ErrExists) {
			return ErrIndexExists
		}
		return fmt.Errorf("ochrevector: reserve index %s: %w", rec.ID, err)
	}
	return nil
}

// Get reads accountID's record for indexID, returning (nil, nil) when absent,
// which is how every caller distinguishes a missing index from a failure.
func (reg *Registry) Get(ctx context.Context, accountID, indexID string) (*Record, error) {
	rec, _, err := reg.store.Get(ctx, registryKey(accountID, indexID))
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// List returns every index record owned by accountID, so one tenant's
// listing never surfaces another's.
func (reg *Registry) List(ctx context.Context, accountID string) ([]Record, error) {
	return reg.store.List(ctx, accountID+"/")
}

// ListAll returns every index record across every account, for the
// reconciler's crash-recovery sweep.
func (reg *Registry) ListAll(ctx context.Context) ([]Record, error) {
	return reg.store.List(ctx, "")
}

// SetState updates accountID's indexID record's State in place, returning
// ErrIndexNotFound if no such record exists.
func (reg *Registry) SetState(ctx context.Context, accountID, indexID, state string) error {
	err := reg.store.Mutate(ctx, registryKey(accountID, indexID), func(rec *Record) (bool, error) {
		rec.State = state
		rec.UpdatedAt = time.Now().UTC()
		return true, nil
	})
	if errors.Is(err, kvstore.ErrNotFound) {
		return ErrIndexNotFound
	}
	if err != nil {
		return fmt.Errorf("ochrevector: update state for index %s: %w", indexID, err)
	}
	return nil
}

// AppendSourceSpec adds spec to accountID's indexID record's SourceSpecs
// unless an identical spec is already present, so a repeated ingest against
// an unchanged source never accumulates duplicate entries (D4: the stored
// source-spec set drives auto-repopulation after a Postgres rebuild).
func (reg *Registry) AppendSourceSpec(ctx context.Context, accountID, indexID string, spec SourceSpec) error {
	err := reg.store.Mutate(ctx, registryKey(accountID, indexID), func(rec *Record) (bool, error) {
		if slices.ContainsFunc(rec.SourceSpecs, func(s SourceSpec) bool { return sourceSpecEqual(s, spec) }) {
			return false, nil
		}
		rec.SourceSpecs = append(rec.SourceSpecs, spec)
		rec.UpdatedAt = time.Now().UTC()
		return true, nil
	})
	if errors.Is(err, kvstore.ErrNotFound) {
		return ErrIndexNotFound
	}
	if err != nil {
		return fmt.Errorf("ochrevector: append source spec for index %s: %w", indexID, err)
	}
	return nil
}

// PurgeAll deletes every index record across every account, so a rebuilt
// appliance's registry never advertises a table that no longer exists.
// Idempotent: an empty bucket is a no-op success.
func (reg *Registry) PurgeAll(ctx context.Context) error {
	return reg.store.DeletePrefix(ctx, "")
}

// Delete removes accountID's indexID record. Idempotent: deleting an
// already-absent record is a no-op success.
func (reg *Registry) Delete(ctx context.Context, accountID, indexID string) error {
	if err := reg.store.Delete(ctx, registryKey(accountID, indexID)); err != nil {
		return fmt.Errorf("ochrevector: delete index %s: %w", indexID, err)
	}
	return nil
}
