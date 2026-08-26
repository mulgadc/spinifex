package handlers_ochrevector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
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
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	IndexID   string `json:"indexId"`
	// DataSourceID ties this job to the bedrock-agent DataSource it was
	// started from, if any: empty for a job started directly against
	// ochre.vector.ingest with no data source involved.
	DataSourceID    string      `json:"dataSourceId,omitempty"`
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
// key "accountID/id", per-account prefix-scan list.
type JobStore struct {
	store *kvstore.Store[JobRecord]
}

// NewJobStore constructs a JobStore over js.
func NewJobStore(js jetstream.JetStream) *JobStore {
	return &JobStore{store: kvstore.New[JobRecord](js, kvstore.Config{
		Name:    jobsBucket,
		History: jobsBucketHistory,
		Missing: "ochrevector: job store has no JetStream client configured",
	})}
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
	rec.AccountID = accountID
	if err := s.store.Create(ctx, jobKey(accountID, rec.ID), &rec); err != nil {
		if errors.Is(err, kvstore.ErrExists) {
			return ErrJobExists
		}
		return fmt.Errorf("ochrevector: reserve job %s: %w", rec.ID, err)
	}
	return nil
}

// Get reads accountID's record for jobID, returning (nil, nil) when absent.
func (s *JobStore) Get(ctx context.Context, accountID, jobID string) (*JobRecord, error) {
	rec, _, err := s.store.Get(ctx, jobKey(accountID, jobID))
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, nil
	}
	return rec, err
}

// List returns every job record owned by accountID, so one tenant's listing
// never surfaces another's.
func (s *JobStore) List(ctx context.Context, accountID string) ([]JobRecord, error) {
	return s.store.List(ctx, accountID+"/")
}

// ListAll returns every job record across every account, for the ingestion
// reconciler's crash-recovery sweep.
func (s *JobStore) ListAll(ctx context.Context) ([]JobRecord, error) {
	return s.store.List(ctx, "")
}

// Update reads accountID's jobID record, applies fn to mutate it in place,
// refreshes UpdatedAt, and writes it back with a revision-guarded (CAS)
// update, mirroring Registry.SetState's pattern for the general case of an
// arbitrary field mutation (progress counters, failed-document list, error).
func (s *JobStore) Update(ctx context.Context, accountID, jobID string, fn func(rec *JobRecord)) error {
	err := s.store.Mutate(ctx, jobKey(accountID, jobID), func(rec *JobRecord) (bool, error) {
		fn(rec)
		rec.UpdatedAt = time.Now().UTC()
		return true, nil
	})
	if errors.Is(err, kvstore.ErrNotFound) {
		return ErrJobNotFound
	}
	if err != nil {
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
	return s.store.DeletePrefix(ctx, "")
}

// Delete removes accountID's jobID record. Idempotent: deleting an
// already-absent record is a no-op success.
func (s *JobStore) Delete(ctx context.Context, accountID, jobID string) error {
	return s.store.Delete(ctx, jobKey(accountID, jobID))
}
