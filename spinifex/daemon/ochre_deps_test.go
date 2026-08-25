// Exercises unexported ochre daemon wiring with no exported surface to
// drive it through.
//
//test:in-package
package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcileFakeBackend is a minimal handlers_ochrevector.VectorBackend
// double for reconcileOchreIndexes tests: only IndexExists/EnsureAccount/
// CreateIndex are exercised, so every other method is an unused stub.
type reconcileFakeBackend struct {
	mu      sync.Mutex
	indexes map[string]bool // key: accountID+"/"+indexID

	ensureAccountCalls []string
	createIndexCalls   []handlers_ochrevector.IndexSpec

	// Error-injection fields for the reconcile error-branch tests below: each
	// makes the matching backend method fail on every call, so a test that
	// sets one exercises exactly the corresponding continue+slog.Warn branch.
	indexExistsErr   error
	ensureAccountErr error
	createIndexErr   error

	// onCreateIndex runs after a successful CreateIndex, letting a test mutate
	// shared state (e.g. delete the registry record) without any production
	// code change, so a later ReconcileFromSource call in the same pass fails.
	onCreateIndex func(ctx context.Context, accountID string, spec handlers_ochrevector.IndexSpec)
}

var _ handlers_ochrevector.VectorBackend = (*reconcileFakeBackend)(nil)

func newReconcileFakeBackend() *reconcileFakeBackend {
	return &reconcileFakeBackend{indexes: map[string]bool{}}
}

func (f *reconcileFakeBackend) Init(_ context.Context) error { return nil }

func (f *reconcileFakeBackend) EnsureAccount(_ context.Context, accountID string) error {
	f.mu.Lock()
	f.ensureAccountCalls = append(f.ensureAccountCalls, accountID)
	err := f.ensureAccountErr
	f.mu.Unlock()
	return err
}

func (f *reconcileFakeBackend) CreateIndex(ctx context.Context, accountID string, spec handlers_ochrevector.IndexSpec) error {
	f.mu.Lock()
	f.createIndexCalls = append(f.createIndexCalls, spec)
	err := f.createIndexErr
	if err == nil {
		f.indexes[accountID+"/"+spec.ID] = true
	}
	onCreateIndex := f.onCreateIndex
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if onCreateIndex != nil {
		onCreateIndex(ctx, accountID, spec)
	}
	return nil
}

func (f *reconcileFakeBackend) IndexExists(_ context.Context, accountID, indexID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.indexExistsErr != nil {
		return false, f.indexExistsErr
	}
	return f.indexes[accountID+"/"+indexID], nil
}

func (f *reconcileFakeBackend) DropIndex(_ context.Context, accountID, indexID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.indexes, accountID+"/"+indexID)
	return nil
}

func (f *reconcileFakeBackend) ReplaceDocument(_ context.Context, _, _, _ string, _ []handlers_ochrevector.VectorRow) error {
	return nil
}

func (f *reconcileFakeBackend) Query(_ context.Context, _, _ string, _ []float32, _ int, _ *handlers_ochrevector.Filter) ([]handlers_ochrevector.QueryResult, error) {
	return nil, nil
}

func (f *reconcileFakeBackend) ensureAccountCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ensureAccountCalls)
}

// TestStartOchreVector_DisabledSkipsConstruction pins the config gate's
// default-off behavior (D-series Stage 5b): with OchreVector.Enabled false
// (the zero value, i.e. every daemon that has not opted in), startOchreVector
// must construct and subscribe nothing, leaving d.ochreVectorService nil and
// d.natsSubscriptions untouched. No JetStream/NATS connection, ctx or
// natsSubscriptions map is set on d here, so any attempt to construct or
// register past the Enabled check would panic (nil natsConn, nil ctx, or a
// nil-map write) -- the test passing at all is itself part of the pin.
func TestStartOchreVector_DisabledSkipsConstruction(t *testing.T) {
	d := &Daemon{
		config: &config.Config{},
	}

	d.startOchreVector()

	assert.Nil(t, d.ochreVectorService)
	assert.Nil(t, d.natsSubscriptions, "disabled path must not touch the subscriptions map")
}

// TestRetryUntilContext_RetriesPastOldCeilingThenSucceeds proves the connect
// loop no longer gives up after the old fixed ceiling: it keeps retrying until
// the appliance is reachable, so RAG heals once a re-adopted appliance returns.
func TestRetryUntilContext_RetriesPastOldCeilingThenSucceeds(t *testing.T) {
	const failuresBeforeSuccess = 7 // deliberately > the old 5-attempt ceiling

	calls, logged := 0, 0
	got, err := retryUntilContext(context.Background(), time.Millisecond, 2*time.Millisecond,
		func(attempt int, backoff time.Duration, err error) { logged++ },
		func() (int, error) {
			calls++
			if calls <= failuresBeforeSuccess {
				return 0, errors.New("appliance not reachable")
			}
			return 42, nil
		})

	require.NoError(t, err)
	assert.Equal(t, 42, got)
	assert.Equal(t, failuresBeforeSuccess+1, calls, "must retry past the old fixed ceiling")
	assert.Equal(t, failuresBeforeSuccess, logged, "each failure reported to log once")
}

// TestRetryUntilContext_StopsOnContextCancel proves daemon shutdown is the
// loop's exit: a cancel while it is backing off returns context.Canceled
// promptly rather than sleeping out the backoff or starting another attempt.
func TestRetryUntilContext_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	got, err := retryUntilContext(ctx, time.Hour, time.Hour,
		func(attempt int, backoff time.Duration, err error) { cancel() },
		func() (int, error) {
			calls++
			return 0, errors.New("still down")
		})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, got)
	assert.Equal(t, 1, calls, "cancel during backoff stops before the next attempt")
}

// TestHandleOchreApplianceTeardown_NilApplianceRefuses proves the handler
// refuses with a clear error rather than a nil-pointer panic when the
// appliance never came up (disabled, still starting, or already torn down by
// an earlier call) -- it must never touch d.ochreVectorService in that case.
func TestHandleOchreApplianceTeardown_NilApplianceRefuses(t *testing.T) {
	d := &Daemon{}

	out, err := d.handleOchreApplianceTeardown(context.Background(), &handlers_ochrevector.TeardownApplianceRequest{}, "")

	require.Error(t, err)
	assert.Nil(t, out)
	assert.Contains(t, err.Error(), "not enabled or not up")
}

const reconcileTestAccount = "111111111111"

// TestReconcileOchreIndexes_RecreatesMissingIndexAndEnqueuesJob proves the
// reconcile pass re-creates a registry index whose pg table is absent (the
// state a fresh, teardown-and-rebuilt appliance leaves) and enqueues exactly
// one PENDING ingest job per persisted SourceSpec.
func TestReconcileOchreIndexes_RecreatesMissingIndexAndEnqueuesJob(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	registry := handlers_ochrevector.NewRegistry(js)
	jobs := handlers_ochrevector.NewJobStore(js)
	backend := newReconcileFakeBackend()
	ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, nil, nil)

	ctx := context.Background()
	now := time.Now().UTC()
	source := handlers_ochrevector.SourceSpec{Bucket: "docs", Prefix: "kb/", ChunkSize: 100, ChunkOverlap: 10, EmbeddingModel: "stub-embed", Dimension: 2}
	require.NoError(t, registry.Reserve(ctx, reconcileTestAccount, handlers_ochrevector.Record{
		ID: "idx-one", Dimension: 2, State: handlers_ochrevector.StateReady,
		SourceSpecs: []handlers_ochrevector.SourceSpec{source}, CreatedAt: now, UpdatedAt: now,
	}))

	d := &Daemon{}
	d.reconcileOchreIndexes(ctx, registry, backend, ingest)

	assert.Equal(t, 1, backend.ensureAccountCallCount(), "must EnsureAccount before CreateIndex")
	require.Len(t, backend.createIndexCalls, 1)
	assert.Equal(t, "idx-one", backend.createIndexCalls[0].ID)

	jobRecs, err := jobs.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, jobRecs, 1, "must enqueue exactly one job for the one persisted SourceSpec")
	assert.Equal(t, handlers_ochrevector.JobStatePending, jobRecs[0].State)
	assert.Equal(t, source, jobRecs[0].Source)
}

// TestReconcileOchreIndexes_ExistingIndexIsNoOp proves an index whose pg
// table already exists (a plain restart, not a teardown) is left alone: no
// EnsureAccount/CreateIndex call and no ingest job enqueued for it.
func TestReconcileOchreIndexes_ExistingIndexIsNoOp(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	registry := handlers_ochrevector.NewRegistry(js)
	jobs := handlers_ochrevector.NewJobStore(js)
	backend := newReconcileFakeBackend()
	ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, nil, nil)

	ctx := context.Background()
	now := time.Now().UTC()
	source := handlers_ochrevector.SourceSpec{Bucket: "docs", Prefix: "kb/", Dimension: 2}
	require.NoError(t, registry.Reserve(ctx, reconcileTestAccount, handlers_ochrevector.Record{
		ID: "idx-one", Dimension: 2, State: handlers_ochrevector.StateReady,
		SourceSpecs: []handlers_ochrevector.SourceSpec{source}, CreatedAt: now, UpdatedAt: now,
	}))
	// Pre-seed the backend as if the table already survived a restart.
	require.NoError(t, backend.CreateIndex(ctx, reconcileTestAccount, handlers_ochrevector.IndexSpec{ID: "idx-one", Dimension: 2}))
	backend.mu.Lock()
	backend.createIndexCalls = nil // reset so the assertion below is reconcile's own calls, not the seed
	backend.mu.Unlock()

	d := &Daemon{}
	d.reconcileOchreIndexes(ctx, registry, backend, ingest)

	assert.Zero(t, backend.ensureAccountCallCount(), "an already-present index must not be re-provisioned")
	assert.Empty(t, backend.createIndexCalls)

	jobRecs, err := jobs.ListAll(ctx)
	require.NoError(t, err)
	assert.Empty(t, jobRecs, "an already-present index must not get a re-ingest job")
}

// TestReconcileOchreIndexes_ErrorsAreLoggedAndSkipped proves each best-effort
// error branch (IndexExists, EnsureAccount, CreateIndex, ReconcileFromSource)
// logs and skips its own record without panicking, rather than failing the
// whole reconcile pass.
func TestReconcileOchreIndexes_ErrorsAreLoggedAndSkipped(t *testing.T) {
	newRecord := func(id string) handlers_ochrevector.Record {
		now := time.Now().UTC()
		return handlers_ochrevector.Record{
			ID: id, Dimension: 2, State: handlers_ochrevector.StateReady,
			SourceSpecs: []handlers_ochrevector.SourceSpec{{Bucket: "docs", Dimension: 2}},
			CreatedAt:   now, UpdatedAt: now,
		}
	}

	t.Run("IndexExists error skips the record", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		registry := handlers_ochrevector.NewRegistry(js)
		jobs := handlers_ochrevector.NewJobStore(js)
		backend := newReconcileFakeBackend()
		backend.indexExistsErr = errors.New("index exists boom")
		ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, nil, nil)

		ctx := context.Background()
		require.NoError(t, registry.Reserve(ctx, reconcileTestAccount, newRecord("idx-exists-err")))

		d := &Daemon{}
		assert.NotPanics(t, func() { d.reconcileOchreIndexes(ctx, registry, backend, ingest) })

		assert.Zero(t, backend.ensureAccountCallCount(), "IndexExists error must skip EnsureAccount")
		assert.Empty(t, backend.createIndexCalls, "IndexExists error must skip CreateIndex")
		jobRecs, err := jobs.ListAll(ctx)
		require.NoError(t, err)
		assert.Empty(t, jobRecs, "IndexExists error must skip the ingest enqueue")
	})

	t.Run("EnsureAccount error skips the record", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		registry := handlers_ochrevector.NewRegistry(js)
		jobs := handlers_ochrevector.NewJobStore(js)
		backend := newReconcileFakeBackend()
		backend.ensureAccountErr = errors.New("ensure account boom")
		ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, nil, nil)

		ctx := context.Background()
		require.NoError(t, registry.Reserve(ctx, reconcileTestAccount, newRecord("idx-ensure-err")))

		d := &Daemon{}
		assert.NotPanics(t, func() { d.reconcileOchreIndexes(ctx, registry, backend, ingest) })

		assert.Equal(t, 1, backend.ensureAccountCallCount(), "EnsureAccount must still be attempted")
		assert.Empty(t, backend.createIndexCalls, "EnsureAccount error must skip CreateIndex")
		jobRecs, err := jobs.ListAll(ctx)
		require.NoError(t, err)
		assert.Empty(t, jobRecs, "EnsureAccount error must skip the ingest enqueue")
	})

	t.Run("CreateIndex error skips the record", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		registry := handlers_ochrevector.NewRegistry(js)
		jobs := handlers_ochrevector.NewJobStore(js)
		backend := newReconcileFakeBackend()
		backend.createIndexErr = errors.New("create index boom")
		ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, nil, nil)

		ctx := context.Background()
		require.NoError(t, registry.Reserve(ctx, reconcileTestAccount, newRecord("idx-create-err")))

		d := &Daemon{}
		assert.NotPanics(t, func() { d.reconcileOchreIndexes(ctx, registry, backend, ingest) })

		require.Len(t, backend.createIndexCalls, 1, "CreateIndex must still be attempted")
		jobRecs, err := jobs.ListAll(ctx)
		require.NoError(t, err)
		assert.Empty(t, jobRecs, "CreateIndex error must skip the ingest enqueue")
	})

	t.Run("ReconcileFromSource error is logged and does not panic", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		registry := handlers_ochrevector.NewRegistry(js)
		jobs := handlers_ochrevector.NewJobStore(js)
		backend := newReconcileFakeBackend()
		ctx := context.Background()
		// Delete the just-created record as a side effect of CreateIndex
		// succeeding, so the ReconcileFromSource call right after it misses
		// the registry and returns ErrIndexNotFound -- no production code
		// change needed to force that failure.
		backend.onCreateIndex = func(ctx context.Context, accountID string, spec handlers_ochrevector.IndexSpec) {
			require.NoError(t, registry.Delete(ctx, accountID, spec.ID))
		}
		ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, nil, nil)

		require.NoError(t, registry.Reserve(ctx, reconcileTestAccount, newRecord("idx-ingest-err")))

		d := &Daemon{}
		assert.NotPanics(t, func() { d.reconcileOchreIndexes(ctx, registry, backend, ingest) })

		require.Len(t, backend.createIndexCalls, 1, "CreateIndex must still run before the ingest enqueue fails")
		jobRecs, err := jobs.ListAll(ctx)
		require.NoError(t, err)
		assert.Empty(t, jobRecs, "ReconcileFromSource error must not leave a job behind")
	})
}
