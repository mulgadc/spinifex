package handlers_ochrevector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Embedder is the local seam for batch text embedding: the same method set
// as gateway_bedrock.Embedder, restated here rather than imported so this
// package never depends on the gateway. The real embedder (an
// *gateway_bedrock adapter) satisfies this structurally; wiring it in is a
// later stage's job.
type Embedder interface {
	Embed(ctx context.Context, modelID string, inputs []string) ([][]float32, error)
}

// TokenLimiter is the local seam for embedder token-budget introspection:
// the same method set as gateway_bedrock.TokenLimiter's MaxInputLength,
// restated here rather than imported so this package never depends on the
// gateway. It is optional -- ingestObject type-asserts s.Embedder against
// it rather than requiring it on Embedder, so a test double Embedder still
// satisfies Embedder alone and falls back to DefaultMaxInputTokens.
type TokenLimiter interface {
	MaxInputLength(ctx context.Context, modelID string) int
}

const (
	// ingestListPageSize bounds each ListObjectsV2 page; StartIngest's caller
	// may point a job at a bucket/prefix far larger than fits in memory at
	// once, so RunJob pages through it via ContinuationToken.
	ingestListPageSize = 1000

	// maxIngestObjectBytes bounds a single source object read into memory
	// during ingestion. An object over this size is a per-document failure
	// (recorded, not fatal to the job), not an unbounded read.
	maxIngestObjectBytes = 32 * 1024 * 1024

	// ingestMaxEmbedRetries bounds the retry attempts for one document's
	// embed call before it counts as a failure -- the "bounded retry" D7
	// requires before a fully-down embedder fails the job.
	ingestMaxEmbedRetries = 3

	// jobStaleAfter bounds how long a RUNNING job may sit before Reconcile
	// treats it as abandoned by a crashed worker and re-drives it from the
	// start -- safe because ReplaceDocument makes reprocessing an
	// already-done document a no-op (D7).
	jobStaleAfter = 5 * time.Minute
)

// ingestRetryBackoff paces retries between embed attempts. A var, not a
// const, so tests can shrink it rather than paying the real delay.
var ingestRetryBackoff = 200 * time.Millisecond

// IngestService orchestrates the ingestion job lifecycle: claiming a job,
// listing a source bucket/prefix, chunking and embedding each object, and
// replacing its rows in the vector backend.
type IngestService struct {
	Jobs     *JobStore
	Registry *Registry
	Backend  VectorBackend
	Store    objectstore.ObjectStore
	Embedder Embedder
}

// NewIngestService constructs an IngestService over its dependencies.
func NewIngestService(jobs *JobStore, registry *Registry, backend VectorBackend, store objectstore.ObjectStore, embedder Embedder) *IngestService {
	return &IngestService{Jobs: jobs, Registry: registry, Backend: backend, Store: store, Embedder: embedder}
}

// StartIngest validates indexID exists for accountID, then reserves a new
// PENDING job for source, tagged with dataSourceID (empty when the caller has
// no bedrock-agent DataSource, e.g. a direct ochre.vector.ingest call). It
// does not run the job -- a scheduler/daemon calls RunJob later (Stage 4);
// tests call RunJob directly.
func (s *IngestService) StartIngest(ctx context.Context, accountID, indexID string, source SourceSpec, dataSourceID string) (*JobRecord, error) {
	idxRec, err := s.Registry.Get(ctx, accountID, indexID)
	if err != nil {
		return nil, err
	}
	if idxRec == nil {
		return nil, ErrIndexNotFound
	}

	now := time.Now().UTC()
	rec := JobRecord{
		ID:           utils.GenerateResourceID("job"),
		IndexID:      indexID,
		DataSourceID: dataSourceID,
		Source:       source,
		State:        JobStatePending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.Jobs.Reserve(ctx, accountID, rec); err != nil {
		return nil, err
	}
	rec.AccountID = accountID
	return &rec, nil
}

// ReconcileFromSource enqueues a PENDING ingest job for accountID/indexID
// from a persisted SourceSpec -- the entry point a startup reconcile pass
// calls to re-populate an index whose backing table survived only in the
// registry, not the freshly-provisioned Postgres. StartIngest's own
// reservation makes a repeat call safe: Sweep/RunJob never double-write.
func (s *IngestService) ReconcileFromSource(ctx context.Context, accountID, indexID string, source SourceSpec) (*JobRecord, error) {
	return s.StartIngest(ctx, accountID, indexID, source, "")
}

// RunJob is the synchronous ingestion worker: lists job.Source, and for each
// object, reads/chunks/embeds/replaces its rows. A per-document failure
// (unreadable object, oversize, exhausted embed retries) is skipped and
// recorded rather than failing the whole job; a fully-unavailable embedder
// (every attempted document's embed calls exhaust retries) fails the job
// instead of reaching READY with everything skipped (D7). On success, the
// job's source spec is recorded on the index registry record (D4).
func (s *IngestService) RunJob(ctx context.Context, job JobRecord) error {
	if err := s.Jobs.SetState(ctx, job.AccountID, job.ID, JobStateRunning); err != nil {
		return err
	}

	keys, err := s.listObjectKeys(ctx, job.Source)
	if err != nil {
		return s.failJob(ctx, job, fmt.Errorf("list source objects: %w", err))
	}

	var failedDocs []FailedDoc
	done := 0
	embedFailures := 0

	for _, key := range keys {
		docErr := s.ingestObject(ctx, job.AccountID, job.IndexID, key, job.Source)
		if docErr != nil {
			slog.WarnContext(ctx, "ochrevector: ingest document failed", "job", job.ID, "key", key, "err", docErr.err)
			failedDocs = append(failedDocs, FailedDoc{SourceKey: key, Reason: docErr.Error()})
			if docErr.fromEmbedder {
				embedFailures++
			}
			continue
		}
		done++
	}

	// A job that ingests nothing must not report READY, regardless of why:
	// an all-embedder-down job gets the specific message, but any other
	// all-failing reason (every GetObject failing, etc.) fails the job too
	// rather than reaching READY with done==0.
	if len(keys) > 0 && done == 0 {
		if embedFailures == len(keys) {
			return s.failJob(ctx, job, fmt.Errorf("ochrevector: embedder unavailable for all %d documents", len(keys)))
		}
		return s.failJob(ctx, job, fmt.Errorf("ochrevector: no documents ingested out of %d", len(keys)))
	}

	if err := s.Registry.AppendSourceSpec(ctx, job.AccountID, job.IndexID, job.Source); err != nil {
		return s.failJob(ctx, job, fmt.Errorf("record source spec: %w", err))
	}

	return s.Jobs.Update(ctx, job.AccountID, job.ID, func(rec *JobRecord) {
		rec.State = JobStateReady
		rec.DocumentsTotal = len(keys)
		rec.DocumentsDone = done
		rec.FailedDocuments = failedDocs
		rec.Error = ""
	})
}

// failJob transitions job to FAILED with cause recorded, and returns cause
// (joined with any error updating the record itself) so the caller sees why.
func (s *IngestService) failJob(ctx context.Context, job JobRecord, cause error) error {
	if updErr := s.Jobs.Update(ctx, job.AccountID, job.ID, func(rec *JobRecord) {
		rec.State = JobStateFailed
		rec.Error = cause.Error()
	}); updErr != nil {
		return errors.Join(cause, updErr)
	}
	return cause
}

// Reconcile re-drives every RUNNING job stale past jobStaleAfter -- the
// crash-recovery sweep a scheduler calls on a timer (no scheduler here).
// Re-running RunJob from the start is safe: ReplaceDocument makes
// reprocessing any already-done document a no-op (D7).
func (s *IngestService) Reconcile(ctx context.Context) error {
	jobs, err := s.Jobs.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("ochrevector: reconcile ingest jobs: list all: %w", err)
	}
	now := time.Now().UTC()
	var errs []error
	for _, job := range jobs {
		if job.State != JobStateRunning {
			continue
		}
		if now.Sub(job.UpdatedAt) < jobStaleAfter {
			continue
		}
		if err := s.RunJob(ctx, job); err != nil {
			errs = append(errs, fmt.Errorf("ochrevector: reconcile job %s: %w", job.ID, err))
		}
	}
	return errors.Join(errs...)
}

// Sweep is the scheduler tick the daemon runs on a timer: it claims and runs
// every PENDING job, and re-drives any RUNNING job a crashed worker abandoned
// (stale past jobStaleAfter). RunJob is idempotent, so a re-run never
// double-writes a document. Reconcile stays the narrower crash-recovery method.
func (s *IngestService) Sweep(ctx context.Context) error {
	jobs, err := s.Jobs.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("ochrevector: sweep ingest jobs: list all: %w", err)
	}
	now := time.Now().UTC()
	var errs []error
	for _, job := range jobs {
		switch {
		case job.State == JobStatePending:
		case job.State == JobStateRunning && now.Sub(job.UpdatedAt) >= jobStaleAfter:
		default:
			continue
		}
		if err := s.RunJob(ctx, job); err != nil {
			errs = append(errs, fmt.Errorf("ochrevector: sweep job %s: %w", job.ID, err))
		}
	}
	return errors.Join(errs...)
}

// ingestDocError is one document's ingestion failure, tagged with whether it
// originated from the embedder (after exhausted retries) so RunJob can tell
// an isolated per-doc miss from every document failing the same way.
type ingestDocError struct {
	err          error
	fromEmbedder bool
}

func (e *ingestDocError) Error() string { return e.err.Error() }
func (e *ingestDocError) Unwrap() error { return e.err }

// ingestObject reads, chunks, embeds and replaces one source object's rows.
func (s *IngestService) ingestObject(ctx context.Context, accountID, indexID, key string, source SourceSpec) *ingestDocError {
	obj, err := s.Store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(source.Bucket), Key: aws.String(key)})
	if err != nil {
		return &ingestDocError{err: fmt.Errorf("get object: %w", err)}
	}
	defer obj.Body.Close()

	data, err := io.ReadAll(io.LimitReader(obj.Body, maxIngestObjectBytes+1))
	if err != nil {
		return &ingestDocError{err: fmt.Errorf("read object body: %w", err)}
	}
	if len(data) > maxIngestObjectBytes {
		return &ingestDocError{err: fmt.Errorf("object exceeds max ingest size (%d bytes)", maxIngestObjectBytes)}
	}

	maxInputTokens := DefaultMaxInputTokens
	if tl, ok := s.Embedder.(TokenLimiter); ok {
		maxInputTokens = tl.MaxInputLength(ctx, source.EmbeddingModel)
	}
	// source.ChunkSize (from Bedrock's FixedSizeChunkingConfiguration.MaxTokens,
	// D3) is an operator token budget, not a rune count; honor it only when
	// it asks for something tighter than the served embedder's real limit --
	// a looser or unset value clamps down to what the embedder actually
	// accepts, so operator config can never reopen the 413 this closes.
	if source.ChunkSize > 0 && source.ChunkSize < maxInputTokens {
		maxInputTokens = source.ChunkSize
	}
	overlapTokens := source.ChunkOverlap
	if overlapTokens < 0 {
		overlapTokens = DefaultChunkOverlapTokens
	}

	var counter TokenCounter
	if tc, ok := s.Embedder.(TokenCounter); ok {
		counter = tc
	}

	chunks := ChunkTextForModel(ctx, string(data), source.EmbeddingModel, maxInputTokens, overlapTokens, counter)
	if len(chunks) == 0 {
		// An empty/whitespace-only document is not an error, but any
		// previously-ingested rows for this key must still be cleared.
		if err := s.Backend.ReplaceDocument(ctx, accountID, indexID, key, nil); err != nil {
			return &ingestDocError{err: fmt.Errorf("replace document: %w", err)}
		}
		return nil
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}

	vectors, err := s.embedWithRetry(ctx, source.EmbeddingModel, texts)
	if err != nil {
		return &ingestDocError{err: fmt.Errorf("embed: %w", err), fromEmbedder: true}
	}
	if len(vectors) != len(chunks) {
		return &ingestDocError{err: fmt.Errorf("embed: vector count mismatch (%d vectors for %d chunks)", len(vectors), len(chunks)), fromEmbedder: true}
	}

	rows := make([]VectorRow, len(chunks))
	for i, c := range chunks {
		rows[i] = VectorRow{Embedding: vectors[i], Chunk: c.Text, Metadata: rowMetadata(source, key), SourceOffset: c.Offset}
	}

	if err := s.Backend.ReplaceDocument(ctx, accountID, indexID, key, rows); err != nil {
		return &ingestDocError{err: fmt.Errorf("replace document: %w", err)}
	}
	return nil
}

// embedWithRetry retries a batch Embed call up to ingestMaxEmbedRetries
// times, the bounded retry D7 requires before a document's embed failure
// counts against the job.
func (s *IngestService) embedWithRetry(ctx context.Context, modelID string, inputs []string) ([][]float32, error) {
	var lastErr error
	for attempt := range ingestMaxEmbedRetries {
		vectors, err := s.Embedder.Embed(ctx, modelID, inputs)
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		if attempt == ingestMaxEmbedRetries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ingestRetryBackoff):
		}
	}
	return nil, lastErr
}

// rowMetadata stamps source's static per-source tags onto one ingested
// row's metadata, plus the originating source key -- without this, D9
// filters have nothing to match against, since a row's metadata would
// otherwise always be empty. Returns a fresh map per call so rows never
// alias the same underlying map.
func rowMetadata(source SourceSpec, key string) map[string]any {
	meta := make(map[string]any, len(source.Metadata)+1)
	for k, v := range source.Metadata {
		meta[k] = v
	}
	meta["sourceKey"] = key
	return meta
}

// listObjectKeys pages through job.Source's bucket/prefix via
// ContinuationToken, returning every object key found.
func (s *IngestService) listObjectKeys(ctx context.Context, source SourceSpec) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.Store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(source.Bucket),
			Prefix:            aws.String(source.Prefix),
			MaxKeys:           aws.Int64(ingestListPageSize),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, obj := range out.Contents {
			keys = append(keys, aws.StringValue(obj.Key))
		}
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}
