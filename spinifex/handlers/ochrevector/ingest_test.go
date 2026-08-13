// Exercises unexported ingest pipeline internals with no exported
// surface to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ingestAccountA = "111111111111"
	ingestBucket   = "docs"
	ingestPrefix   = "kb/"
)

// stubEmbedder is a controllable Embedder test double: it can fail every
// call, fail only calls whose input contains a marker substring, and always
// returns a deterministic (non-random) vector per input so ingest_test can
// assert exact row contents.
type stubEmbedder struct {
	mu             sync.Mutex
	calls          int
	failAll        bool
	failIfContains string
}

var _ Embedder = (*stubEmbedder)(nil)

func (e *stubEmbedder) Embed(_ context.Context, _ string, inputs []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()

	if e.failAll {
		return nil, errors.New("stub embedder: unavailable")
	}
	if e.failIfContains != "" {
		for _, in := range inputs {
			if strings.Contains(in, e.failIfContains) {
				return nil, errors.New("stub embedder: rejected input")
			}
		}
	}
	vectors := make([][]float32, len(inputs))
	for i, in := range inputs {
		vectors[i] = []float32{float32(len(in)), float32(i)}
	}
	return vectors, nil
}

func (e *stubEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// newIngestTestSetup wires an IngestService over fresh in-memory stores, with
// accountA already owning a READY index "idx-one".
func newIngestTestSetup(t *testing.T) (*IngestService, *Registry, *fakeBackend, *objectstore.MemoryObjectStore, *stubEmbedder) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	registry := NewRegistry(js)
	now := time.Now().UTC()
	require.NoError(t, registry.Reserve(context.Background(), ingestAccountA, Record{
		ID: "idx-one", Dimension: 2, EmbeddingModel: "stub-embed", State: StateCreating, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, registry.SetState(context.Background(), ingestAccountA, "idx-one", StateReady))

	backend := newFakeBackend()
	store := objectstore.NewMemoryObjectStore()
	embedder := &stubEmbedder{}
	jobs := NewJobStore(js)

	svc := NewIngestService(jobs, registry, backend, store, embedder)
	return svc, registry, backend, store, embedder
}

func testSource() SourceSpec {
	return SourceSpec{Bucket: ingestBucket, Prefix: ingestPrefix, ChunkSize: 100, ChunkOverlap: 10, EmbeddingModel: "stub-embed", Dimension: 2}
}

func putObject(t *testing.T, store *objectstore.MemoryObjectStore, key, body string) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(ingestBucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(body),
	})
	require.NoError(t, err)
}

func TestStartIngest_MissingIndexErrors(t *testing.T) {
	svc, _, _, _, _ := newIngestTestSetup(t)
	ctx := context.Background()

	_, err := svc.StartIngest(ctx, ingestAccountA, "idx-does-not-exist", testSource(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexNotFound)
}

func TestStartIngest_ReturnsPendingJobWithoutRunning(t *testing.T) {
	svc, _, backend, store, embedder := newIngestTestSetup(t)
	ctx := context.Background()
	putObject(t, store, ingestPrefix+"doc1.txt", "hello world")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, JobStatePending, job.State)
	assert.Equal(t, ingestAccountA, job.AccountID)
	assert.Equal(t, "idx-one", job.IndexID)

	// StartIngest must not itself run the job.
	assert.Equal(t, 0, embedder.callCount())
	assert.Equal(t, 0, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", ingestPrefix+"doc1.txt"))
}

// TestStartIngest_StampsDataSourceID proves a non-empty dataSourceID argument
// lands on the reserved job record unchanged, and an empty one (a direct
// ochre.vector.ingest caller with no bedrock-agent DataSource) leaves it
// empty rather than defaulting to some placeholder value.
func TestStartIngest_StampsDataSourceID(t *testing.T) {
	svc, _, _, _, _ := newIngestTestSetup(t)
	ctx := context.Background()

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "ds-1")
	require.NoError(t, err)
	assert.Equal(t, "ds-1", job.DataSourceID)

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ds-1", got.DataSourceID)

	noDSJob, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	assert.Empty(t, noDSJob.DataSourceID)
}

func TestRunJob_HappyPath(t *testing.T) {
	svc, registry, backend, store, embedder := newIngestTestSetup(t)
	ctx := context.Background()

	putObject(t, store, ingestPrefix+"doc1.txt", "the quick brown fox jumps over the lazy dog")
	putObject(t, store, ingestPrefix+"doc2.txt", "a second, different document body")
	putObject(t, store, ingestPrefix+"doc3.txt", "and a third one, for good measure")

	source := testSource()
	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", source, "")
	require.NoError(t, err)

	require.NoError(t, svc.RunJob(ctx, *job))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateReady, got.State)
	assert.Equal(t, 3, got.DocumentsTotal)
	assert.Equal(t, 3, got.DocumentsDone)
	assert.Empty(t, got.FailedDocuments)
	assert.Empty(t, got.Error)

	for _, key := range []string{ingestPrefix + "doc1.txt", ingestPrefix + "doc2.txt", ingestPrefix + "doc3.txt"} {
		assert.Equal(t, 1, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", key), "key %s", key)
		rows := backend.documentRows(ingestAccountA, "idx-one", key)
		require.NotEmpty(t, rows, "key %s must have rows", key)
		for _, row := range rows {
			assert.NotEmpty(t, row.Embedding)
			assert.NotEmpty(t, row.Chunk)
		}
	}
	assert.Positive(t, embedder.callCount())

	idxRec, err := registry.Get(ctx, ingestAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, idxRec)
	require.Len(t, idxRec.SourceSpecs, 1, "source spec must be appended exactly once")
	assert.Equal(t, source, idxRec.SourceSpecs[0])
}

// TestRunJob_IdempotentRerun proves running the same job twice keeps the
// backend's per-key row set replaced, not doubled, and appends the source
// spec only once despite two successful runs.
func TestRunJob_IdempotentRerun(t *testing.T) {
	svc, registry, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()
	key := ingestPrefix + "doc1.txt"
	putObject(t, store, key, "the quick brown fox jumps over the lazy dog")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)

	require.NoError(t, svc.RunJob(ctx, *job))
	firstRows := backend.documentRows(ingestAccountA, "idx-one", key)
	require.NotEmpty(t, firstRows)

	// Re-fetch the job (state now READY) and run it again, as Reconcile
	// would for a stale RUNNING job.
	rerun, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NoError(t, svc.RunJob(ctx, *rerun))

	assert.Equal(t, 2, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", key), "ReplaceDocument is called again on rerun")
	secondRows := backend.documentRows(ingestAccountA, "idx-one", key)
	assert.Len(t, secondRows, len(firstRows), "the row set must be replaced, not doubled")

	idxRec, err := registry.Get(ctx, ingestAccountA, "idx-one")
	require.NoError(t, err)
	require.Len(t, idxRec.SourceSpecs, 1, "an identical source spec must not be appended twice")
}

// TestRunJob_PerDocumentFailureIsSkippedAndRecorded proves one document
// whose embed call fails is recorded in FailedDocuments while the rest of
// the job proceeds to READY.
func TestRunJob_PerDocumentFailureIsSkippedAndRecorded(t *testing.T) {
	svc, _, backend, store, embedder := newIngestTestSetup(t)
	ctx := context.Background()
	origBackoff := ingestRetryBackoff
	ingestRetryBackoff = time.Millisecond
	t.Cleanup(func() { ingestRetryBackoff = origBackoff })

	goodKey := ingestPrefix + "good.txt"
	badKey := ingestPrefix + "bad.txt"
	putObject(t, store, goodKey, "a perfectly fine document body")
	putObject(t, store, badKey, "this document contains POISON and will fail to embed")
	embedder.failIfContains = "POISON"

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.RunJob(ctx, *job))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateReady, got.State)
	assert.Equal(t, 2, got.DocumentsTotal)
	assert.Equal(t, 1, got.DocumentsDone)
	require.Len(t, got.FailedDocuments, 1)
	assert.Equal(t, badKey, got.FailedDocuments[0].SourceKey)
	assert.NotEmpty(t, got.FailedDocuments[0].Reason)

	assert.Equal(t, 1, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", goodKey))
	assert.Equal(t, 0, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", badKey))
}

// TestRunJob_EmbedderFullyDownFailsJob proves that when every document's
// embed call fails, the job reaches FAILED (after the bounded retry) rather
// than READY with everything skipped.
func TestRunJob_EmbedderFullyDownFailsJob(t *testing.T) {
	svc, _, backend, store, embedder := newIngestTestSetup(t)
	ctx := context.Background()
	origBackoff := ingestRetryBackoff
	ingestRetryBackoff = time.Millisecond
	t.Cleanup(func() { ingestRetryBackoff = origBackoff })

	putObject(t, store, ingestPrefix+"doc1.txt", "document one")
	putObject(t, store, ingestPrefix+"doc2.txt", "document two")
	embedder.failAll = true

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)

	err = svc.RunJob(ctx, *job)
	require.Error(t, err)

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateFailed, got.State)
	assert.NotEmpty(t, got.Error)

	assert.Equal(t, 0, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", ingestPrefix+"doc1.txt"))
	// Every doc must have been retried up to the bound before giving up.
	assert.Equal(t, 2*ingestMaxEmbedRetries, embedder.callCount())
}

func TestRunJob_EmptyDocumentClearsExistingRows(t *testing.T) {
	svc, _, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()
	key := ingestPrefix + "empty.txt"
	putObject(t, store, key, "   \n\t  ")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.RunJob(ctx, *job))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStateReady, got.State)
	assert.Equal(t, 1, got.DocumentsDone)
	assert.Equal(t, 1, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", key))
	assert.Empty(t, backend.documentRows(ingestAccountA, "idx-one", key))
}

// forceJobUpdatedAt seeds jobID's UpdatedAt directly via the underlying KV
// bucket, bypassing JobStore.Update (which always refreshes UpdatedAt to
// now), so a test can simulate a record a crashed worker left stale --
// mirroring appliance_test.go's raw-KV seeding for the same reason.
func forceJobUpdatedAt(t *testing.T, store *JobStore, accountID, jobID string, when time.Time) {
	t.Helper()
	ctx := context.Background()
	kv, err := store.bucket(ctx)
	require.NoError(t, err)
	entry, err := kv.Get(ctx, jobKey(accountID, jobID))
	require.NoError(t, err)
	var rec JobRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	rec.UpdatedAt = when
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = kv.Update(ctx, jobKey(accountID, jobID), data, entry.Revision())
	require.NoError(t, err)
}

func TestIngestService_Reconcile_RedrivesStaleRunningJob(t *testing.T) {
	svc, _, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()
	key := ingestPrefix + "doc1.txt"
	putObject(t, store, key, "a document that a crashed worker never finished")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.Jobs.SetState(ctx, ingestAccountA, job.ID, JobStateRunning))

	// Force the record's UpdatedAt stale, as a crashed worker would leave it.
	forceJobUpdatedAt(t, svc.Jobs, ingestAccountA, job.ID, time.Now().UTC().Add(-2*jobStaleAfter))

	require.NoError(t, svc.Reconcile(ctx))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateReady, got.State)
	assert.Equal(t, 1, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", key))
}

func TestIngestService_Reconcile_LeavesFreshRunningJobAlone(t *testing.T) {
	svc, _, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()
	key := ingestPrefix + "doc1.txt"
	putObject(t, store, key, "still in flight")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.Jobs.SetState(ctx, ingestAccountA, job.ID, JobStateRunning))

	require.NoError(t, svc.Reconcile(ctx))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateRunning, got.State, "a RUNNING job within the grace period must be left alone")
	assert.Equal(t, 0, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", key))
}

// failingGetObjectStore wraps a MemoryObjectStore so ListObjectsV2 lists
// real objects but every GetObject call fails, simulating a
// non-embedder-related reason every document fails for (e.g. a source
// bucket gone unreachable mid-job).
type failingGetObjectStore struct {
	*objectstore.MemoryObjectStore
}

func (f *failingGetObjectStore) GetObject(_ context.Context, _ *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	return nil, errors.New("simulated GetObject failure")
}

// TestRunJob_AllDocumentsFailNonEmbedReason_FailsJob proves a job whose
// every document fails for a reason other than the embedder (every
// GetObject failing here) reaches FAILED, not READY with done==0 -- the
// old check only caught the all-embed-failures case, so a job that ingests
// literally nothing for any other reason used to sail through to READY.
func TestRunJob_AllDocumentsFailNonEmbedReason_FailsJob(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()

	registry := NewRegistry(js)
	now := time.Now().UTC()
	require.NoError(t, registry.Reserve(ctx, ingestAccountA, Record{
		ID: "idx-one", Dimension: 2, EmbeddingModel: "stub-embed", State: StateCreating, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, registry.SetState(ctx, ingestAccountA, "idx-one", StateReady))

	mem := objectstore.NewMemoryObjectStore()
	putObject(t, mem, ingestPrefix+"doc1.txt", "one")
	putObject(t, mem, ingestPrefix+"doc2.txt", "two")
	store := &failingGetObjectStore{MemoryObjectStore: mem}

	backend := newFakeBackend()
	embedder := &stubEmbedder{}
	jobs := NewJobStore(js)
	svc := NewIngestService(jobs, registry, backend, store, embedder)

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)

	err = svc.RunJob(ctx, *job)
	require.Error(t, err)

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateFailed, got.State)
	assert.NotEmpty(t, got.Error)
	assert.Equal(t, 0, got.DocumentsDone)

	idxRec, err := registry.Get(ctx, ingestAccountA, "idx-one")
	require.NoError(t, err)
	require.NotNil(t, idxRec)
	assert.Empty(t, idxRec.SourceSpecs, "a job that ingested nothing must not append its source spec")

	// The embedder was never even reached -- these are GetObject failures.
	assert.Equal(t, 0, embedder.callCount())
}

// TestRunJob_StampsSourceMetadataOntoRows proves ingestObject stamps
// SourceSpec.Metadata's static per-source tags, plus the source key, onto
// every ingested row -- without this, D9 filters have nothing to match.
func TestRunJob_StampsSourceMetadataOntoRows(t *testing.T) {
	svc, _, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()
	key := ingestPrefix + "doc1.txt"
	putObject(t, store, key, "a document with static tags to filter on")

	source := testSource()
	source.Metadata = map[string]string{"category": "handbook", "team": "platform"}

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", source, "")
	require.NoError(t, err)
	require.NoError(t, svc.RunJob(ctx, *job))

	rows := backend.documentRows(ingestAccountA, "idx-one", key)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		assert.Equal(t, "handbook", row.Metadata["category"])
		assert.Equal(t, "platform", row.Metadata["team"])
		assert.Equal(t, key, row.Metadata["sourceKey"], "the source key must also be stamped, for a filter to narrow by originating document")
	}

	// A second document's rows carry the same source tags but its own key --
	// a filter on category=handbook selects both; a filter on sourceKey
	// selects the one document.
	otherKey := ingestPrefix + "doc2.txt"
	putObject(t, store, otherKey, "a second document, same source tags")
	job2, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", source, "")
	require.NoError(t, err)
	require.NoError(t, svc.RunJob(ctx, *job2))

	otherRows := backend.documentRows(ingestAccountA, "idx-one", otherKey)
	require.NotEmpty(t, otherRows)
	for _, row := range otherRows {
		assert.Equal(t, "handbook", row.Metadata["category"], "both documents share the source's static tags")
		assert.Equal(t, otherKey, row.Metadata["sourceKey"], "each document's rows carry its own source key")
	}
}

func TestIngestService_Reconcile_IgnoresPendingAndTerminalJobs(t *testing.T) {
	svc, _, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()
	putObject(t, store, ingestPrefix+"doc1.txt", "pending, never run")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	forceJobUpdatedAt(t, svc.Jobs, ingestAccountA, job.ID, time.Now().UTC().Add(-2*jobStaleAfter))

	require.NoError(t, svc.Reconcile(ctx))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatePending, got.State, "Reconcile only re-drives RUNNING jobs, not PENDING ones")
	assert.Equal(t, 0, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", ingestPrefix+"doc1.txt"))
}

// TestIngestService_Sweep_RunsPendingJobToReady proves Sweep is what actually
// drives a fresh PENDING job to completion -- the gap Reconcile deliberately
// leaves open, since nothing else ever calls RunJob for a PENDING job.
func TestIngestService_Sweep_RunsPendingJobToReady(t *testing.T) {
	svc, _, backend, store, embedder := newIngestTestSetup(t)
	ctx := context.Background()
	key := ingestPrefix + "doc1.txt"
	putObject(t, store, key, "a pending job the scheduler must claim and run")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.Equal(t, JobStatePending, job.State)

	require.NoError(t, svc.Sweep(ctx))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateReady, got.State)
	assert.Equal(t, 1, got.DocumentsDone)
	assert.Equal(t, 1, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", key))
	assert.Positive(t, embedder.callCount())
}

// TestIngestService_Sweep_RedrivesStaleRunningJob proves Sweep also covers
// Reconcile's crash-recovery case, so a single scheduler tick handles both.
func TestIngestService_Sweep_RedrivesStaleRunningJob(t *testing.T) {
	svc, _, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()
	key := ingestPrefix + "doc1.txt"
	putObject(t, store, key, "a stale running job the scheduler must re-drive")

	job, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.Jobs.SetState(ctx, ingestAccountA, job.ID, JobStateRunning))
	forceJobUpdatedAt(t, svc.Jobs, ingestAccountA, job.ID, time.Now().UTC().Add(-2*jobStaleAfter))

	require.NoError(t, svc.Sweep(ctx))

	got, err := svc.Jobs.Get(ctx, ingestAccountA, job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, JobStateReady, got.State)
	assert.Equal(t, 1, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", key))
}

// TestIngestService_Sweep_LeavesFreshRunningAndTerminalJobsAlone proves Sweep
// does not touch a RUNNING job still within its grace period, nor a job
// already in a terminal state (READY or FAILED).
func TestIngestService_Sweep_LeavesFreshRunningAndTerminalJobsAlone(t *testing.T) {
	svc, _, backend, store, _ := newIngestTestSetup(t)
	ctx := context.Background()

	freshKey := ingestPrefix + "fresh.txt"
	putObject(t, store, freshKey, "still in flight, must not be touched")
	freshJob, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.Jobs.SetState(ctx, ingestAccountA, freshJob.ID, JobStateRunning))

	readyJob, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.Jobs.SetState(ctx, ingestAccountA, readyJob.ID, JobStateReady))

	failedJob, err := svc.StartIngest(ctx, ingestAccountA, "idx-one", testSource(), "")
	require.NoError(t, err)
	require.NoError(t, svc.Jobs.SetState(ctx, ingestAccountA, failedJob.ID, JobStateFailed))

	require.NoError(t, svc.Sweep(ctx))

	gotFresh, err := svc.Jobs.Get(ctx, ingestAccountA, freshJob.ID)
	require.NoError(t, err)
	require.NotNil(t, gotFresh)
	assert.Equal(t, JobStateRunning, gotFresh.State, "a RUNNING job within the grace period must be left alone")

	gotReady, err := svc.Jobs.Get(ctx, ingestAccountA, readyJob.ID)
	require.NoError(t, err)
	require.NotNil(t, gotReady)
	assert.Equal(t, JobStateReady, gotReady.State)

	gotFailed, err := svc.Jobs.Get(ctx, ingestAccountA, failedJob.ID)
	require.NoError(t, err)
	require.NotNil(t, gotFailed)
	assert.Equal(t, JobStateFailed, gotFailed.State)

	assert.Equal(t, 0, backend.replaceDocumentCallCount(ingestAccountA, "idx-one", freshKey))
}
