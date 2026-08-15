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

// jobsBucket is the cluster-replicated KV bucket holding ingestion job
// records, per-account, mirroring registryBucket's pattern.
const jobsBucket = "ochre-vector-jobs"

// jobsBucketHistory keeps one revision per key: a job record is one JSON
// document mutated in place, not a series.
const jobsBucketHistory = 1

// Ingestion job lifecycle states.
const (
	JobStatePending = "PENDING"
	JobStateRunning = "RUNNING"
	JobStateReady   = "READY"
	JobStateFailed  = "FAILED"
)

// ErrJobExists reports that Reserve lost the single-writer claim on a job id
// already reserved for that account.
var ErrJobExists = errors.New("ochrevector: job already exists")

// ErrJobNotFound reports that an operation targeted an account+job pair with
// no job record.
var ErrJobNotFound = errors.New("ochrevector: job not found")

// FailedDoc records one document an ingestion job skipped rather than
// letting fail the whole job (D7): its source key and why it was skipped.
type FailedDoc struct {
	SourceKey string `json:"sourceKey"`
	Reason    string `json:"reason"`
}

// JobRecord is one ingestion job: PENDING (reserved, not yet run) ->
// RUNNING -> READY (completed, possibly with per-document failures) or
// FAILED (the job itself could not complete, e.g. embedder fully down).
type JobRecord struct {
	ID              string      `json:"id"`
	AccountID       string      `json:"accountId"`
	IndexID         string      `json:"indexId"`
	Source          SourceSpec  `json:"source"`
	State           string      `json:"state"`
	DocumentsTotal  int         `json:"documentsTotal"`
	DocumentsDone   int         `json:"documentsDone"`
	FailedDocuments []FailedDoc `json:"failedDocuments,omitempty"`
	Error           string      `json:"error,omitempty"`
	CreatedAt       time.Time   `json:"createdAt"`
	UpdatedAt       time.Time   `json:"updatedAt"`
}

// JobStore persists JobRecords in the ochre-vector-jobs JetStream KV bucket,
// mirroring Registry's lazy get-or-create bucket, key "accountID/id",
// per-account prefix-scan list.
type JobStore struct {
	js jetstream.JetStream

	mu sync.Mutex
	kv jetstream.KeyValue
}

// NewJobStore constructs a JobStore over js.
func NewJobStore(js jetstream.JetStream) *JobStore {
	return &JobStore{js: js}
}

// bucket lazily opens (or creates) the jobs KV bucket, caching the handle.
func (s *JobStore) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	if s.js == nil {
		return nil, errors.New("ochrevector: job store has no JetStream client configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		return s.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucket(ctx, s.js, jobsBucket, jobsBucketHistory)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	return kv, nil
}

// jobKey scopes every record to its owning account, so a foreign account's
// raw-id guess can never collide with another tenant's key.
func jobKey(accountID, jobID string) string {
	return accountID + "/" + jobID
}

// Reserve atomically claims rec.ID for accountID: the create-only KV write is
// the single-writer mutex (mirrors Registry.Reserve), so two concurrent
// starts of the same job id race safely and exactly one wins. rec.AccountID
// is stamped from accountID, overriding whatever the caller set.
func (s *JobStore) Reserve(ctx context.Context, accountID string, rec JobRecord) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	rec.AccountID = accountID
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ochrevector: encode job record %s: %w", rec.ID, err)
	}
	if _, err := kv.Create(ctx, jobKey(accountID, rec.ID), data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrJobExists
		}
		return fmt.Errorf("ochrevector: reserve job %s: %w", rec.ID, err)
	}
	return nil
}

// Get reads accountID's record for jobID, returning (nil, nil) when absent.
func (s *JobStore) Get(ctx context.Context, accountID, jobID string) (*JobRecord, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	return getJobRecord(ctx, kv, jobKey(accountID, jobID))
}

// getJobRecord reads and decodes one record, returning (nil, nil) when absent.
func getJobRecord(ctx context.Context, kv jetstream.KeyValue, key string) (*JobRecord, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: kv get %s: %w", key, err)
	}
	var rec JobRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("ochrevector: decode %s: %w", key, err)
	}
	return &rec, nil
}

// List returns every job record owned by accountID, so one tenant's listing
// never surfaces another's.
func (s *JobStore) List(ctx context.Context, accountID string) ([]JobRecord, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	return listJobRecords(ctx, kv, accountID+"/")
}

// ListAll returns every job record across every account, for the ingestion
// reconciler's crash-recovery sweep.
func (s *JobStore) ListAll(ctx context.Context) ([]JobRecord, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	return listJobRecords(ctx, kv, "")
}

// listJobRecords walks every key with the given prefix ("" matches
// everything) and decodes each into a JobRecord, skipping any key that
// disappears between the key listing and the read.
func listJobRecords(ctx context.Context, kv jetstream.KeyValue, prefix string) ([]JobRecord, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("ochrevector: list keys: %w", err)
	}
	var out []JobRecord
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rec, err := getJobRecord(ctx, kv, key)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	return out, nil
}

// Update reads accountID's jobID record, applies fn to mutate it in place,
// refreshes UpdatedAt, and writes it back with a revision-guarded (CAS)
// update, mirroring Registry.SetState's pattern for the general case of an
// arbitrary field mutation (progress counters, failed-document list, error).
func (s *JobStore) Update(ctx context.Context, accountID, jobID string, fn func(rec *JobRecord)) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	key := jobKey(accountID, jobID)
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrJobNotFound
		}
		return fmt.Errorf("ochrevector: kv get %s: %w", key, err)
	}
	var rec JobRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return fmt.Errorf("ochrevector: decode %s: %w", key, err)
	}
	fn(&rec)
	rec.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ochrevector: encode job record %s: %w", rec.ID, err)
	}
	if _, err := kv.Update(ctx, key, data, entry.Revision()); err != nil {
		return fmt.Errorf("ochrevector: update job %s: %w", jobID, err)
	}
	return nil
}

// SetState is Update specialized to the common single-field state
// transition, mirroring Registry.SetState.
func (s *JobStore) SetState(ctx context.Context, accountID, jobID, state string) error {
	return s.Update(ctx, accountID, jobID, func(rec *JobRecord) {
		rec.State = state
	})
}

// PurgeAll deletes every job record across every account, so a rebuilt
// appliance's job history never references data that no longer exists.
// Idempotent: an empty bucket is a no-op success.
func (s *JobStore) PurgeAll(ctx context.Context) error {
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
			return fmt.Errorf("ochrevector: purge job %s: %w", key, err)
		}
	}
	return nil
}

// Delete removes accountID's jobID record. Idempotent: deleting an
// already-absent record is a no-op success.
func (s *JobStore) Delete(ctx context.Context, accountID, jobID string) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	key := jobKey(accountID, jobID)
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("ochrevector: delete job %s: %w", jobID, err)
	}
	return nil
}
