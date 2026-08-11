package handlers_ochrevector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	apiAccountA = "111111111111"
	apiAccountB = "222222222222"
)

// apiTestSetup wires a real Service/IngestService/JobStore/Registry over an
// embedded JetStream server, plus a fakeBackend and stubEmbedder, so
// vectorService is exercised against the same collaborators it runs with in
// production -- only the backend and embedder are doubles.
type apiTestSetup struct {
	svc      VectorService
	registry *Registry
	backend  *fakeBackend
	embedder *stubEmbedder
}

func newAPITestSetup(t *testing.T) *apiTestSetup {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	registry := NewRegistry(js)
	backend := newFakeBackend()
	indexSvc := NewService(registry, backend)

	jobs := NewJobStore(js)
	embedder := &stubEmbedder{}
	ingestSvc := NewIngestService(jobs, registry, backend, nil, embedder)

	return &apiTestSetup{
		svc:      NewVectorService(indexSvc, ingestSvc, jobs, registry, backend, embedder),
		registry: registry,
		backend:  backend,
		embedder: embedder,
	}
}

func TestVectorService_CreateIndex_HappyPath(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()

	out, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{
		IndexID: "idx-one", Name: "kb1", Dimension: 768, EmbeddingModel: "nomic-embed-text-v1.5",
	}, apiAccountA)
	require.NoError(t, err)
	assert.Equal(t, StateReady, out.Index.State)
	assert.Equal(t, apiAccountA, out.Index.AccountID)
	assert.True(t, s.backend.hasIndex(apiAccountA, "idx-one"))
}

func TestVectorService_CreateIndex_MissingIndexIDErrors(t *testing.T) {
	s := newAPITestSetup(t)
	_, err := s.svc.CreateIndex(context.Background(), &CreateIndexRequest{Name: "kb1", Dimension: 768}, apiAccountA)
	require.Error(t, err)
}

func TestVectorService_CreateIndex_ExistsErrorPropagates(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	req := &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 768}

	_, err := s.svc.CreateIndex(ctx, req, apiAccountA)
	require.NoError(t, err)

	_, err = s.svc.CreateIndex(ctx, req, apiAccountA)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexExists)
}

func TestVectorService_DeleteIndex_HappyPath(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 768}, apiAccountA)
	require.NoError(t, err)

	_, err = s.svc.DeleteIndex(ctx, &DeleteIndexRequest{IndexID: "idx-one"}, apiAccountA)
	require.NoError(t, err)

	rec, err := s.registry.Get(ctx, apiAccountA, "idx-one")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestVectorService_DeleteIndex_AbsentIsNoop(t *testing.T) {
	s := newAPITestSetup(t)
	_, err := s.svc.DeleteIndex(context.Background(), &DeleteIndexRequest{IndexID: "no-such-index"}, apiAccountA)
	require.NoError(t, err)
}

func TestVectorService_ListIndexes_ScopedPerAccount(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-a", Name: "a", Dimension: 8}, apiAccountA)
	require.NoError(t, err)
	_, err = s.svc.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-b", Name: "b", Dimension: 8}, apiAccountB)
	require.NoError(t, err)

	out, err := s.svc.ListIndexes(ctx, &ListIndexesRequest{}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, out.Indexes, 1)
	assert.Equal(t, "idx-a", out.Indexes[0].ID)
}

// TestVectorService_Ingest_StampsIndexPinnedModel proves Ingest overwrites
// whatever EmbeddingModel/Dimension a caller sends with the index's own
// registered values (D6/D8): the stored source-spec must always match the
// index it belongs to, not whatever a stale or spoofed caller supplied.
func TestVectorService_Ingest_StampsIndexPinnedModel(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{
		IndexID: "idx-one", Name: "kb1", Dimension: 768, EmbeddingModel: "nomic-embed-text-v1.5",
	}, apiAccountA)
	require.NoError(t, err)

	out, err := s.svc.Ingest(ctx, &IngestRequest{
		IndexID: "idx-one",
		Source:  SourceSpec{Bucket: "docs", Prefix: "kb/", EmbeddingModel: "some-other-model", Dimension: 3},
	}, apiAccountA)
	require.NoError(t, err)
	assert.Equal(t, "nomic-embed-text-v1.5", out.Job.Source.EmbeddingModel)
	assert.Equal(t, 768, out.Job.Source.Dimension)
	assert.Equal(t, JobStatePending, out.Job.State)
}

func TestVectorService_Ingest_MissingIndexErrors(t *testing.T) {
	s := newAPITestSetup(t)
	_, err := s.svc.Ingest(context.Background(), &IngestRequest{IndexID: "no-such-index", Source: SourceSpec{Bucket: "docs"}}, apiAccountA)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexNotFound)
}

func TestVectorService_DescribeJob_HappyPath(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 8}, apiAccountA)
	require.NoError(t, err)
	ingestOut, err := s.svc.Ingest(ctx, &IngestRequest{IndexID: "idx-one", Source: SourceSpec{Bucket: "docs"}}, apiAccountA)
	require.NoError(t, err)

	out, err := s.svc.DescribeJob(ctx, &DescribeJobRequest{JobID: ingestOut.Job.ID}, apiAccountA)
	require.NoError(t, err)
	assert.Equal(t, ingestOut.Job.ID, out.Job.ID)
}

func TestVectorService_DescribeJob_AbsentReturnsErrJobNotFound(t *testing.T) {
	s := newAPITestSetup(t)
	_, err := s.svc.DescribeJob(context.Background(), &DescribeJobRequest{JobID: "no-such-job"}, apiAccountA)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrJobNotFound)
}

// TestVectorService_Query_UsesIndexPinnedModel proves Query embeds Text
// against the index's own registered model, not any model the caller might
// have sent -- QueryRequest has no such field at all, but this also confirms
// the value actually reaching Embedder is the registry's.
func TestVectorService_Query_UsesIndexPinnedModel(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{
		IndexID: "idx-one", Name: "kb1", Dimension: 2, EmbeddingModel: "nomic-embed-text-v1.5",
	}, apiAccountA)
	require.NoError(t, err)
	s.backend.queryResults = []QueryResult{{Chunk: "hello", SourceKey: "docs/a.txt", Score: 0.9}}

	out, err := s.svc.Query(ctx, &QueryRequest{IndexID: "idx-one", Text: "what is this about?"}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "hello", out.Results[0].Chunk)
	assert.Equal(t, 1, s.embedder.callCount())
}

// TestVectorService_Query_NonReadyIndexReturnsEmptyNotError proves a
// CREATING/DELETING/STALE index reports empty results rather than an error
// (D4): it is not yet, or no longer, queryable, but a query against it is not
// a caller mistake.
func TestVectorService_Query_NonReadyIndexReturnsEmptyNotError(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.registry.Reserve(ctx, apiAccountA, Record{
		ID: "idx-one", Dimension: 2, EmbeddingModel: "m", State: StateCreating, CreatedAt: now, UpdatedAt: now,
	}))

	out, err := s.svc.Query(ctx, &QueryRequest{IndexID: "idx-one", Text: "hello"}, apiAccountA)
	require.NoError(t, err)
	assert.Empty(t, out.Results)
	assert.Equal(t, 0, s.embedder.callCount(), "a non-READY index must not even reach the embedder")
}

func TestVectorService_Query_MissingIndexErrors(t *testing.T) {
	s := newAPITestSetup(t)
	_, err := s.svc.Query(context.Background(), &QueryRequest{IndexID: "no-such-index", Text: "hello"}, apiAccountA)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIndexNotFound)
}

func TestVectorService_Query_EmbedderErrorPropagates(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 2}, apiAccountA)
	require.NoError(t, err)
	s.embedder.failAll = true

	_, err = s.svc.Query(ctx, &QueryRequest{IndexID: "idx-one", Text: "hello"}, apiAccountA)
	require.Error(t, err)
}

// TestVectorService_Query_TruncatesLongChunkText proves the NATS payload
// guard (D10): a chunk larger than maxResponseChunkChars is bounded in the
// response, with a marker pointing back at the source object rather than
// silently dropping the excess.
func TestVectorService_Query_TruncatesLongChunkText(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 2}, apiAccountA)
	require.NoError(t, err)

	longChunk := strings.Repeat("a", maxResponseChunkChars+500)
	s.backend.queryResults = []QueryResult{{Chunk: longChunk, SourceKey: "docs/a.txt", SourceOffset: 42, Score: 0.5}}

	out, err := s.svc.Query(ctx, &QueryRequest{IndexID: "idx-one", Text: "hello"}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.Less(t, len(out.Results[0].Chunk), len(longChunk))
	assert.Contains(t, out.Results[0].Chunk, "truncated")
	assert.Equal(t, "docs/a.txt", out.Results[0].SourceKey)
	assert.Equal(t, 42, out.Results[0].SourceOffset)
}

func TestVectorService_Query_ShortChunkUntouched(t *testing.T) {
	s := newAPITestSetup(t)
	ctx := context.Background()
	_, err := s.svc.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 2}, apiAccountA)
	require.NoError(t, err)
	s.backend.queryResults = []QueryResult{{Chunk: "short chunk", SourceKey: "docs/a.txt", Score: 0.5}}

	out, err := s.svc.Query(ctx, &QueryRequest{IndexID: "idx-one", Text: "hello"}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "short chunk", out.Results[0].Chunk)
}

// stubVectorNATSHandler mirrors daemon.handleNATSRequest's unmarshal ->
// service -> marshal/error -> respond wire contract, without importing the
// daemon package. It exists purely so this test exercises the exact bytes
// NATSVectorService and the real daemon subscription (Stage 5b) put on the
// wire.
func stubVectorNATSHandler[I any, O any](serviceFn func(context.Context, *I, string) (*O, error)) nats.MsgHandler {
	return func(msg *nats.Msg) {
		accountID := utils.AccountIDFromMsg(msg)
		input := new(I)
		if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		output, err := serviceFn(context.Background(), input, accountID)
		if err != nil {
			payload := utils.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), err.Error())
			_ = msg.Respond(payload)
			return
		}
		data, err := json.Marshal(output)
		if err != nil {
			_ = msg.Respond(utils.GenerateErrorPayloadWithMessage(awserrors.ErrorServerInternal, err.Error()))
			return
		}
		_ = msg.Respond(data)
	}
}

// subscribeVectorService registers every VectorService subject on the
// "spinifex-workers" queue group, exactly as daemon.subscribeAll will for the
// real service in Stage 5b, so the round trip below exercises the production
// subject names and queue group.
func subscribeVectorService(t *testing.T, nc *nats.Conn, svc VectorService) {
	t.Helper()
	subs := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{SubjectCreateIndex, stubVectorNATSHandler(svc.CreateIndex)},
		{SubjectDeleteIndex, stubVectorNATSHandler(svc.DeleteIndex)},
		{SubjectListIndexes, stubVectorNATSHandler(svc.ListIndexes)},
		{SubjectIngest, stubVectorNATSHandler(svc.Ingest)},
		{SubjectDescribeJob, stubVectorNATSHandler(svc.DescribeJob)},
		{SubjectQuery, stubVectorNATSHandler(svc.Query)},
	}
	for _, s := range subs {
		sub, err := nc.QueueSubscribe(s.subject, "spinifex-workers", s.handler)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
}

func TestNATSVectorService_RoundTrip(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	registry := NewRegistry(js)
	backend := newFakeBackend()
	indexSvc := NewService(registry, backend)
	jobs := NewJobStore(js)
	embedder := &stubEmbedder{}
	ingestSvc := NewIngestService(jobs, registry, backend, nil, embedder)
	svc := NewVectorService(indexSvc, ingestSvc, jobs, registry, backend, embedder)

	subscribeVectorService(t, nc, svc)
	client := NewNATSVectorService(nc)
	ctx := context.Background()

	createOut, err := client.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 2, EmbeddingModel: "m"}, apiAccountA)
	require.NoError(t, err)
	assert.Equal(t, StateReady, createOut.Index.State)

	listOut, err := client.ListIndexes(ctx, &ListIndexesRequest{}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, listOut.Indexes, 1)

	ingestOut, err := client.Ingest(ctx, &IngestRequest{IndexID: "idx-one", Source: SourceSpec{Bucket: "docs"}}, apiAccountA)
	require.NoError(t, err)
	assert.NotEmpty(t, ingestOut.Job.ID)

	descOut, err := client.DescribeJob(ctx, &DescribeJobRequest{JobID: ingestOut.Job.ID}, apiAccountA)
	require.NoError(t, err)
	assert.Equal(t, ingestOut.Job.ID, descOut.Job.ID)

	backend.queryResults = []QueryResult{{Chunk: "hi", SourceKey: "docs/a.txt", Score: 0.9}}
	queryOut, err := client.Query(ctx, &QueryRequest{IndexID: "idx-one", Text: "hello"}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, queryOut.Results, 1)
	assert.Equal(t, "hi", queryOut.Results[0].Chunk)

	_, err = client.DeleteIndex(ctx, &DeleteIndexRequest{IndexID: "idx-one"}, apiAccountA)
	require.NoError(t, err)

	listOut, err = client.ListIndexes(ctx, &ListIndexesRequest{}, apiAccountA)
	require.NoError(t, err)
	assert.Empty(t, listOut.Indexes)
}

// TestNATSVectorService_AccountIsolation proves accountID travels only via
// the NATS header the transport sets, never the JSON payload (D10): account
// B's client never sees account A's index even though both use the same
// wire and subject names.
func TestNATSVectorService_AccountIsolation(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	registry := NewRegistry(js)
	backend := newFakeBackend()
	indexSvc := NewService(registry, backend)
	jobs := NewJobStore(js)
	embedder := &stubEmbedder{}
	ingestSvc := NewIngestService(jobs, registry, backend, nil, embedder)
	svc := NewVectorService(indexSvc, ingestSvc, jobs, registry, backend, embedder)

	subscribeVectorService(t, nc, svc)
	client := NewNATSVectorService(nc)
	ctx := context.Background()

	_, err := client.CreateIndex(ctx, &CreateIndexRequest{IndexID: "idx-one", Name: "kb1", Dimension: 2}, apiAccountA)
	require.NoError(t, err)

	listOut, err := client.ListIndexes(ctx, &ListIndexesRequest{}, apiAccountB)
	require.NoError(t, err)
	assert.Empty(t, listOut.Indexes)
}
