package handlers_ochrevector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
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
// bucket, mirroring GuardrailStore's gateway-direct-KV pattern (lazy
// get-or-create bucket, key "accountID/id", per-account prefix-scan list).
type Registry struct {
	js jetstream.JetStream

	mu sync.Mutex
	kv jetstream.KeyValue
}

// NewRegistry constructs a Registry over js.
func NewRegistry(js jetstream.JetStream) *Registry {
	return &Registry{js: js}
}

// bucket lazily opens (or creates) the registry KV bucket, caching the
// handle.
func (reg *Registry) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	if reg.js == nil {
		return nil, errors.New("ochrevector: registry has no JetStream client configured")
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.kv != nil {
		return reg.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucket(ctx, reg.js, registryBucket, registryBucketHistory)
	if err != nil {
		return nil, err
	}
	reg.kv = kv
	return kv, nil
}

// registryKey scopes every record to its owning account, so a foreign
// account's raw-id guess can never collide with another tenant's key.
func registryKey(accountID, indexID string) string {
	return accountID + "/" + indexID
}

// Reserve atomically claims rec.ID for accountID: the create-only KV write is
// the single-writer mutex (mirrors handlers/rds's identifier claim via
// jetstream.ErrKeyExists), so two concurrent creates of the same id race
// safely and exactly one wins. rec.AccountID is stamped from accountID,
// overriding whatever the caller set.
func (reg *Registry) Reserve(ctx context.Context, accountID string, rec Record) error {
	kv, err := reg.bucket(ctx)
	if err != nil {
		return err
	}
	rec.AccountID = accountID
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ochrevector: encode index record %s: %w", rec.ID, err)
	}
	if _, err := kv.Create(ctx, registryKey(accountID, rec.ID), data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrIndexExists
		}
		return fmt.Errorf("ochrevector: reserve index %s: %w", rec.ID, err)
	}
	return nil
}

// Get reads accountID's record for indexID, returning (nil, nil) when absent.
func (reg *Registry) Get(ctx context.Context, accountID, indexID string) (*Record, error) {
	kv, err := reg.bucket(ctx)
	if err != nil {
		return nil, err
	}
	return getRecord(ctx, kv, registryKey(accountID, indexID))
}

// getRecord reads and decodes one record, returning (nil, nil) when absent.
func getRecord(ctx context.Context, kv jetstream.KeyValue, key string) (*Record, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: kv get %s: %w", key, err)
	}
	var rec Record
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("ochrevector: decode %s: %w", key, err)
	}
	return &rec, nil
}

// List returns every index record owned by accountID, so one tenant's
// listing never surfaces another's.
func (reg *Registry) List(ctx context.Context, accountID string) ([]Record, error) {
	kv, err := reg.bucket(ctx)
	if err != nil {
		return nil, err
	}
	return listRecords(ctx, kv, accountID+"/")
}

// ListAll returns every index record across every account, for the
// reconciler's crash-recovery sweep.
func (reg *Registry) ListAll(ctx context.Context) ([]Record, error) {
	kv, err := reg.bucket(ctx)
	if err != nil {
		return nil, err
	}
	return listRecords(ctx, kv, "")
}

// listRecords walks every key with the given prefix ("" matches everything)
// and decodes each into a Record, skipping any key that disappears between
// the key listing and the read.
func listRecords(ctx context.Context, kv jetstream.KeyValue, prefix string) ([]Record, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: list keys: %w", err)
	}
	var out []Record
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rec, err := getRecord(ctx, kv, key)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	return out, nil
}

// SetState updates accountID's indexID record's State in place, returning
// ErrIndexNotFound if no such record exists.
func (reg *Registry) SetState(ctx context.Context, accountID, indexID, state string) error {
	kv, err := reg.bucket(ctx)
	if err != nil {
		return err
	}
	key := registryKey(accountID, indexID)
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrIndexNotFound
		}
		return fmt.Errorf("ochrevector: kv get %s: %w", key, err)
	}
	var rec Record
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return fmt.Errorf("ochrevector: decode %s: %w", key, err)
	}
	rec.State = state
	rec.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ochrevector: encode index record %s: %w", rec.ID, err)
	}
	if _, err := kv.Update(ctx, key, data, entry.Revision()); err != nil {
		return fmt.Errorf("ochrevector: update state for index %s: %w", indexID, err)
	}
	return nil
}

// Delete removes accountID's indexID record. Idempotent: deleting an
// already-absent record is a no-op success.
func (reg *Registry) Delete(ctx context.Context, accountID, indexID string) error {
	kv, err := reg.bucket(ctx)
	if err != nil {
		return err
	}
	key := registryKey(accountID, indexID)
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("ochrevector: delete index %s: %w", indexID, err)
	}
	return nil
}
