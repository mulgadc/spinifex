// Exercises unexported rerank orchestration internals with no exported
// surface to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubReranker is a controllable Reranker test double: it can fail every
// call, or reorder docs by a caller-supplied score function (defaulting to
// reversing the candidate order, so a rerank pass is visibly distinguishable
// from the KNN order fakeBackend hands back).
type stubReranker struct {
	failAll   bool
	failErr   error
	order     func(docs []string) []int
	lastQuery string
	lastDocs  []string
	calls     int
}

var _ Reranker = (*stubReranker)(nil)

func (r *stubReranker) Rerank(_ context.Context, query string, docs []string) ([]int, error) {
	r.calls++
	r.lastQuery = query
	r.lastDocs = docs
	if r.failAll {
		if r.failErr != nil {
			return nil, r.failErr
		}
		return nil, errors.New("stub reranker: unavailable")
	}
	if r.order != nil {
		return r.order(docs), nil
	}
	// Default: reverse order, so tests can tell a reranked result apart
	// from the KNN order without needing a custom scoring function.
	indices := make([]int, len(docs))
	for i := range docs {
		indices[i] = len(docs) - 1 - i
	}
	return indices, nil
}

// newAPITestSetupWithReranker mirrors newAPITestSetup but wires reranker
// (non-nil, unlike the default setup) so these tests exercise Query's
// over-fetch + rerank path.
func newAPITestSetupWithReranker(t *testing.T, reranker Reranker) *apiTestSetup {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	registry := NewRegistry(js)
	backend := newFakeBackend()
	indexSvc := NewService(registry, backend)

	jobs := NewJobStore(js)
	embedder := &stubEmbedder{}
	ingestSvc := NewIngestService(jobs, registry, backend, nil, embedder)

	return &apiTestSetup{
		svc:      NewVectorService(indexSvc, ingestSvc, jobs, registry, backend, embedder, reranker),
		registry: registry,
		backend:  backend,
		embedder: embedder,
	}
}

func createRerankTestIndex(t *testing.T, s *apiTestSetup) {
	t.Helper()
	_, err := s.svc.CreateIndex(context.Background(), &CreateIndexRequest{
		IndexID: "idx-one", Name: "kb1", Dimension: 2,
	}, apiAccountA)
	require.NoError(t, err)
}

// TestVectorService_Query_RerankReordersCandidates proves a configured
// reranker's own order wins over the backend's KNN order, and the returned
// results are truncated to the caller's requested k.
func TestVectorService_Query_RerankReordersCandidates(t *testing.T) {
	reranker := &stubReranker{}
	s := newAPITestSetupWithReranker(t, reranker)
	createRerankTestIndex(t, s)

	s.backend.queryResults = []QueryResult{
		{Chunk: "first", SourceKey: "docs/a.txt", Score: 0.9},
		{Chunk: "second", SourceKey: "docs/b.txt", Score: 0.8},
		{Chunk: "third", SourceKey: "docs/c.txt", Score: 0.7},
	}

	out, err := s.svc.Query(context.Background(), &QueryRequest{IndexID: "idx-one", Text: "hello", K: 2}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, out.Results, 2)
	// stubReranker's default order reverses the candidates: third, second,
	// first -- truncated to k=2 gives third then second, the opposite of
	// the KNN order fakeBackend handed back.
	assert.Equal(t, "third", out.Results[0].Chunk)
	assert.Equal(t, "second", out.Results[1].Chunk)
	assert.Equal(t, "hello", reranker.lastQuery)
}

// TestVectorService_Query_RerankOverFetchesFromBackend proves Query asks
// the backend for more than k candidates when a reranker is configured, so
// the reranker has real headroom to improve on the KNN order.
func TestVectorService_Query_RerankOverFetchesFromBackend(t *testing.T) {
	reranker := &stubReranker{}
	s := newAPITestSetupWithReranker(t, reranker)
	createRerankTestIndex(t, s)

	canned := make([]QueryResult, rerankMaxCandidates)
	for i := range canned {
		canned[i] = QueryResult{Chunk: "chunk", SourceKey: "docs/a.txt", Score: 1.0}
	}
	s.backend.queryResults = canned

	_, err := s.svc.Query(context.Background(), &QueryRequest{IndexID: "idx-one", Text: "hello", K: 2}, apiAccountA)
	require.NoError(t, err)
	assert.Equal(t, rerankFetchK(2), s.backend.lastQueryK)
	assert.Greater(t, s.backend.lastQueryK, 2, "over-fetch must request more than k from the backend")
}

// TestVectorService_Query_NoRerankerFallsBackToKNNOrder proves an unset
// reranker (the default, matching every deployment that has not configured
// a rerank endpoint) leaves Query at the backend's own KNN order, and asks
// the backend for exactly k, not an over-fetched count.
func TestVectorService_Query_NoRerankerFallsBackToKNNOrder(t *testing.T) {
	s := newAPITestSetup(t) // reranker is nil in the default setup.
	createRerankTestIndex(t, s)

	s.backend.queryResults = []QueryResult{
		{Chunk: "first", SourceKey: "docs/a.txt", Score: 0.9},
		{Chunk: "second", SourceKey: "docs/b.txt", Score: 0.8},
	}

	out, err := s.svc.Query(context.Background(), &QueryRequest{IndexID: "idx-one", Text: "hello", K: 2}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, out.Results, 2)
	assert.Equal(t, "first", out.Results[0].Chunk)
	assert.Equal(t, "second", out.Results[1].Chunk)
	assert.Equal(t, 2, s.backend.lastQueryK, "no reranker configured must not over-fetch")
}

// TestVectorService_Query_RerankErrorFallsBackToKNNOrder proves a reranker
// that errors never fails the request: Query falls back to the backend's
// own KNN top-k rather than propagating the rerank failure.
func TestVectorService_Query_RerankErrorFallsBackToKNNOrder(t *testing.T) {
	reranker := &stubReranker{failAll: true}
	s := newAPITestSetupWithReranker(t, reranker)
	createRerankTestIndex(t, s)

	s.backend.queryResults = []QueryResult{
		{Chunk: "first", SourceKey: "docs/a.txt", Score: 0.9},
		{Chunk: "second", SourceKey: "docs/b.txt", Score: 0.8},
	}

	out, err := s.svc.Query(context.Background(), &QueryRequest{IndexID: "idx-one", Text: "hello", K: 2}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, out.Results, 2)
	assert.Equal(t, "first", out.Results[0].Chunk)
	assert.Equal(t, "second", out.Results[1].Chunk)
	assert.Equal(t, 1, reranker.calls)
}

// TestVectorService_Query_RerankCapsCandidateDocLength proves a candidate
// chunk larger than rerankMaxDocRunes is truncated before being sent to the
// reranker, bounding the /rerank request body regardless of chunk size.
func TestVectorService_Query_RerankCapsCandidateDocLength(t *testing.T) {
	reranker := &stubReranker{}
	s := newAPITestSetupWithReranker(t, reranker)
	createRerankTestIndex(t, s)

	longChunk := strings.Repeat("a", rerankMaxDocRunes+500)
	s.backend.queryResults = []QueryResult{{Chunk: longChunk, SourceKey: "docs/a.txt", Score: 0.9}}

	_, err := s.svc.Query(context.Background(), &QueryRequest{IndexID: "idx-one", Text: "hello", K: 1}, apiAccountA)
	require.NoError(t, err)
	require.Len(t, reranker.lastDocs, 1)
	assert.Len(t, []rune(reranker.lastDocs[0]), rerankMaxDocRunes)
}

// TestRerankFetchK proves the over-fetch factor/cap arithmetic directly.
func TestRerankFetchK(t *testing.T) {
	tests := []struct {
		name string
		k    int
		want int
	}{
		{"small k scales by factor", 2, 8},
		{"scaled value clamped to max candidates", 10, rerankMaxCandidates},
		{"k above max candidates passes through unover-fetched", 30, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, rerankFetchK(tt.k))
		})
	}
}

// TestRerankTopK_NilRerankerReturnsKNNOrderTruncated proves the helper's own
// nil-reranker fallback directly, independent of vectorService.Query.
func TestRerankTopK_NilRerankerReturnsKNNOrderTruncated(t *testing.T) {
	results := []QueryResult{{Chunk: "a"}, {Chunk: "b"}, {Chunk: "c"}}
	got := rerankTopK(context.Background(), nil, "query", results, 2)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Chunk)
	assert.Equal(t, "b", got[1].Chunk)
}

// TestRerankTopK_OutOfRangeIndexFallsBackToKNNOrder proves a malformed
// Reranker response (an index outside the candidate set) is treated the
// same as a hard Rerank error: fall back rather than panic or silently drop
// results.
func TestRerankTopK_OutOfRangeIndexFallsBackToKNNOrder(t *testing.T) {
	results := []QueryResult{{Chunk: "a"}, {Chunk: "b"}}
	reranker := &stubReranker{order: func([]string) []int { return []int{5} }}
	got := rerankTopK(context.Background(), reranker, "query", results, 2)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Chunk)
	assert.Equal(t, "b", got[1].Chunk)
}
