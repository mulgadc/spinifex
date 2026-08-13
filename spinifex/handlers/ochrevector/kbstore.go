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

// kbBucket is the cluster-replicated KV bucket holding bedrock-agent
// knowledge-base records, per-account, mirroring registryBucket's pattern.
// Unencrypted: knowledge-base metadata (name, status, bound index) carries no
// secrets, unlike the credentials gateway_bedrock.CredentialStore protects.
const kbBucket = "ochre-kb"

// dataSourceBucket is the cluster-replicated KV bucket holding bedrock-agent
// data-source records, per-account, mirroring kbBucket.
const dataSourceBucket = "ochre-kb-datasource"

// kbBucketHistory and dataSourceBucketHistory keep one revision per key: a
// record is one JSON document mutated in place, not a series.
const (
	kbBucketHistory         = 1
	dataSourceBucketHistory = 1
)

// ErrKBExists reports that Create lost the single-writer claim on a knowledge
// base id already reserved for that account.
var ErrKBExists = errors.New("ochrevector: knowledge base already exists")

// ErrKBNotFound reports that an operation targeted an account+knowledge-base
// pair with no record.
var ErrKBNotFound = errors.New("ochrevector: knowledge base not found")

// ErrDataSourceExists reports that Create lost the single-writer claim on a
// data-source id already reserved for that account.
var ErrDataSourceExists = errors.New("ochrevector: data source already exists")

// ErrDataSourceNotFound reports that an operation targeted an
// account+data-source pair with no record.
var ErrDataSourceNotFound = errors.New("ochrevector: data source not found")

// KBRecord is one bedrock-agent knowledge base: the AWS-shaped resource
// wrapping a single bound vector Index (D1: one KB binds exactly one index).
// Status reuses the vector index lifecycle strings (StateReady et al, see
// registry.go) rather than minting a parallel AWS-shaped enum here -- the
// gateway layer translates to AWS's KnowledgeBaseStatus wire values.
type KBRecord struct {
	ID             string `json:"id"`
	AccountID      string `json:"accountId"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Status         string `json:"status"`
	EmbeddingModel string `json:"embeddingModel"`
	// EmbeddingModelArn is the caller's original embeddingModelArn, stored
	// verbatim for round-trip echo; EmbeddingModel is the bare model id
	// derived from it (see embeddingModelIDFromARN) that CreateIndex and the
	// embedder actually key on.
	EmbeddingModelArn string `json:"embeddingModelArn,omitempty"`
	Dimension         int    `json:"dimension"`
	IndexID           string `json:"indexId"`
	// RoleArn and StorageConfigJSON are accepted-and-stubbed (D5): stored
	// verbatim so Get/List echo back what the caller sent, but never acted
	// on -- pgvector abstracts away AWS's OpenSearch/RDS storage binding.
	// StorageConfigJSON is an opaque JSON blob (the AWS StorageConfiguration
	// shape) rather than a typed field, so this package stays free of an
	// aws-sdk-go dependency; the gateway layer decodes/encodes it.
	RoleArn           string          `json:"roleArn,omitempty"`
	StorageConfigJSON json.RawMessage `json:"storageConfig,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

// DataSourceRecord is one bedrock-agent data source: the S3 + chunking
// ingestion config for one KnowledgeBaseID, stored as a SourceSpec so
// StartIngestionJob can build an IngestRequest from it directly. Status
// reuses the vector index lifecycle strings, mirroring KBRecord.
type DataSourceRecord struct {
	ID              string     `json:"id"`
	AccountID       string     `json:"accountId"`
	KnowledgeBaseID string     `json:"knowledgeBaseId"`
	Name            string     `json:"name"`
	Description     string     `json:"description,omitempty"`
	Status          string     `json:"status"`
	Source          SourceSpec `json:"source"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

// KBStore persists KBRecords in the ochre-kb JetStream KV bucket, mirroring
// Registry's lazy get-or-create bucket, key "accountID/id", per-account
// prefix-scan list.
type KBStore struct {
	js jetstream.JetStream

	mu sync.Mutex
	kv jetstream.KeyValue
}

// NewKBStore constructs a KBStore over js.
func NewKBStore(js jetstream.JetStream) *KBStore {
	return &KBStore{js: js}
}

// bucket lazily opens (or creates) the KB KV bucket, caching the handle.
func (s *KBStore) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	if s.js == nil {
		return nil, errors.New("ochrevector: knowledge base store has no JetStream client configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		return s.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucket(ctx, s.js, kbBucket, kbBucketHistory)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	return kv, nil
}

// kbKey scopes every record to its owning account, so a foreign account's raw
// id guess can never collide with another tenant's key.
func kbKey(accountID, id string) string {
	return accountID + "/" + id
}

// Create atomically claims rec.ID for accountID: the create-only KV write is
// the single-writer mutex (mirrors Registry.Reserve), so two concurrent
// creates of the same id race safely and exactly one wins. rec.AccountID is
// stamped from accountID, overriding whatever the caller set.
func (s *KBStore) Create(ctx context.Context, accountID string, rec KBRecord) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	rec.AccountID = accountID
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ochrevector: encode knowledge base %s: %w", rec.ID, err)
	}
	if _, err := kv.Create(ctx, kbKey(accountID, rec.ID), data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrKBExists
		}
		return fmt.Errorf("ochrevector: reserve knowledge base %s: %w", rec.ID, err)
	}
	return nil
}

// Get reads accountID's record for id, returning (nil, nil) when absent.
func (s *KBStore) Get(ctx context.Context, accountID, id string) (*KBRecord, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := kv.Get(ctx, kbKey(accountID, id))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: kv get %s: %w", kbKey(accountID, id), err)
	}
	var rec KBRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("ochrevector: decode %s: %w", kbKey(accountID, id), err)
	}
	return &rec, nil
}

// List returns every knowledge base owned by accountID, so one tenant's
// listing never surfaces another's.
func (s *KBStore) List(ctx context.Context, accountID string) ([]KBRecord, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: list keys: %w", err)
	}
	prefix := accountID + "/"
	var out []KBRecord
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("ochrevector: kv get %s: %w", key, err)
		}
		var rec KBRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("ochrevector: decode %s: %w", key, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// Delete removes accountID's id record. Idempotent: deleting an
// already-absent record is a no-op success.
func (s *KBStore) Delete(ctx context.Context, accountID, id string) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	key := kbKey(accountID, id)
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("ochrevector: delete knowledge base %s: %w", id, err)
	}
	return nil
}

// PurgeAll deletes every knowledge base record across every account.
// Idempotent: an empty bucket is a no-op success.
func (s *KBStore) PurgeAll(ctx context.Context) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("ochrevector: list keys: %w", err)
	}
	for _, key := range keys {
		if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("ochrevector: purge knowledge base %s: %w", key, err)
		}
	}
	return nil
}

// DataSourceStore persists DataSourceRecords in the ochre-kb-datasource
// JetStream KV bucket, mirroring KBStore.
type DataSourceStore struct {
	js jetstream.JetStream

	mu sync.Mutex
	kv jetstream.KeyValue
}

// NewDataSourceStore constructs a DataSourceStore over js.
func NewDataSourceStore(js jetstream.JetStream) *DataSourceStore {
	return &DataSourceStore{js: js}
}

// bucket lazily opens (or creates) the data-source KV bucket, caching the
// handle.
func (s *DataSourceStore) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	if s.js == nil {
		return nil, errors.New("ochrevector: data source store has no JetStream client configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		return s.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucket(ctx, s.js, dataSourceBucket, dataSourceBucketHistory)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	return kv, nil
}

// dataSourceKey scopes every record to its owning account, so a foreign
// account's raw id guess can never collide with another tenant's key.
func dataSourceKey(accountID, id string) string {
	return accountID + "/" + id
}

// Create atomically claims rec.ID for accountID, mirroring KBStore.Create.
func (s *DataSourceStore) Create(ctx context.Context, accountID string, rec DataSourceRecord) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	rec.AccountID = accountID
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ochrevector: encode data source %s: %w", rec.ID, err)
	}
	if _, err := kv.Create(ctx, dataSourceKey(accountID, rec.ID), data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrDataSourceExists
		}
		return fmt.Errorf("ochrevector: reserve data source %s: %w", rec.ID, err)
	}
	return nil
}

// Get reads accountID's record for id, returning (nil, nil) when absent.
func (s *DataSourceStore) Get(ctx context.Context, accountID, id string) (*DataSourceRecord, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := kv.Get(ctx, dataSourceKey(accountID, id))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: kv get %s: %w", dataSourceKey(accountID, id), err)
	}
	var rec DataSourceRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("ochrevector: decode %s: %w", dataSourceKey(accountID, id), err)
	}
	return &rec, nil
}

// List returns every data source owned by accountID, across every knowledge
// base, so one tenant's listing never surfaces another's.
func (s *DataSourceStore) List(ctx context.Context, accountID string) ([]DataSourceRecord, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: list keys: %w", err)
	}
	prefix := accountID + "/"
	var out []DataSourceRecord
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("ochrevector: kv get %s: %w", key, err)
		}
		var rec DataSourceRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("ochrevector: decode %s: %w", key, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// ListByKnowledgeBase returns accountID's data sources scoped to
// knowledgeBaseID, the shape ListDataSources needs.
func (s *DataSourceStore) ListByKnowledgeBase(ctx context.Context, accountID, knowledgeBaseID string) ([]DataSourceRecord, error) {
	all, err := s.List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]DataSourceRecord, 0, len(all))
	for _, rec := range all {
		if rec.KnowledgeBaseID == knowledgeBaseID {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Delete removes accountID's id record. Idempotent: deleting an
// already-absent record is a no-op success.
func (s *DataSourceStore) Delete(ctx context.Context, accountID, id string) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	key := dataSourceKey(accountID, id)
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("ochrevector: delete data source %s: %w", id, err)
	}
	return nil
}

// PurgeAll deletes every data source record across every account. Idempotent:
// an empty bucket is a no-op success.
func (s *DataSourceStore) PurgeAll(ctx context.Context) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("ochrevector: list keys: %w", err)
	}
	for _, key := range keys {
		if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("ochrevector: purge data source %s: %w", key, err)
		}
	}
	return nil
}
