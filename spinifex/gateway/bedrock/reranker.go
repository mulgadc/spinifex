package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	rerankPath = "/rerank"

	// maxRerankResponseBytes bounds the /rerank response body read into
	// memory, mirroring the embeddings introspection endpoints' bound.
	maxRerankResponseBytes = 1 << 20
)

// DefaultRerankModel is Ochre's GPU-served cross-encoder reranker,
// co-resident in the demo bundle alongside DefaultEmbeddingModel.
const DefaultRerankModel = "bge-reranker-v2-m3"

// Reranker reorders a candidate document set against a query using a
// self-hosted cross-encoder (TEI-compatible POST /rerank), returning doc
// indices into the caller's docs slice, best match first.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]int, error)
}

// rerankRequest is TEI's own POST /rerank request wire shape: a query plus
// the candidate texts to score against it.
type rerankRequest struct {
	Query string   `json:"query"`
	Texts []string `json:"texts"`
}

// rerankResult is one candidate's score, keyed back to the caller's docs
// slice by Index. TEI does not document the response as pre-sorted, so this
// adapter sorts by Score itself rather than trusting response order.
type rerankResult struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// rerankProvider serves cross-encoder reranking over a TEI-compatible
// POST /rerank endpoint, resolving modelID's endpoint through the shared
// EndpointResolver the same way embeddingsProvider resolves the embedder.
type rerankProvider struct {
	modelID          string
	endpointResolver EndpointResolver
	httpClient       *http.Client
}

var _ Reranker = (*rerankProvider)(nil)

// newRerankProvider constructs a rerankProvider resolving modelID's endpoint
// via endpointResolver on every call, mirroring newEmbeddingsProvider.
func newRerankProvider(endpointResolver EndpointResolver, modelID string) *rerankProvider {
	return &rerankProvider{
		modelID:          modelID,
		endpointResolver: endpointResolver,
		httpClient:       &http.Client{Timeout: providerHTTPTimeout},
	}
}

// NewReranker is newRerankProvider's exported constructor, for callers
// outside this package (e.g. the daemon's Ochre vector store wiring) that
// need a Reranker without reaching into gateway_bedrock's unexported types.
func NewReranker(endpointResolver EndpointResolver, modelID string) Reranker {
	return newRerankProvider(endpointResolver, modelID)
}

// Rerank POSTs query and docs to the reranker model's resolved /rerank
// endpoint, returning doc indices ordered best-match first. Empty docs skip
// the call; an unresolved endpoint returns ModelNotReadyException.
func (p *rerankProvider) Rerank(ctx context.Context, query string, docs []string) ([]int, error) {
	if len(docs) == 0 {
		return nil, nil
	}

	baseURL, ok, err := p.endpointResolver.Endpoint(ctx, p.modelID)
	if err != nil {
		slog.Debug("rerank: endpoint resolution failed", "model", p.modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if !ok {
		return nil, errors.New(awserrors.ErrorModelNotReadyException)
	}

	reqBody, err := json.Marshal(rerankRequest{Query: query, Texts: docs})
	if err != nil {
		slog.Error("rerank: failed to marshal request", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+rerankPath, bytes.NewReader(reqBody))
	if err != nil {
		slog.Error("rerank: failed to build request", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		slog.Debug("rerank: request failed", "endpoint", baseURL, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRerankResponseBytes))
	if err != nil {
		slog.Debug("rerank: failed to read response body", "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Debug("rerank: upstream error", "status", resp.StatusCode, "body", string(respBody))
		return nil, errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	var results []rerankResult
	if err := json.Unmarshal(respBody, &results); err != nil {
		slog.Debug("rerank: failed to parse response", "err", err)
		return nil, errors.New(awserrors.ErrorModelErrorException)
	}

	indices, err := orderByScore(len(docs), results)
	if err != nil {
		slog.Debug("rerank: malformed response shape", "err", err)
		return nil, errors.New(awserrors.ErrorModelErrorException)
	}
	return indices, nil
}

// orderByScore validates one result per doc index in [0,n) with no
// duplicates -- guarding against unrequested top_n truncation -- then
// returns indices sorted by descending score.
func orderByScore(n int, results []rerankResult) ([]int, error) {
	if len(results) != n {
		return nil, errors.New("rerank result count mismatch")
	}
	seen := make([]bool, n)
	for _, r := range results {
		if r.Index < 0 || r.Index >= n || seen[r.Index] {
			return nil, errors.New("rerank index out of range or duplicated")
		}
		seen[r.Index] = true
	}

	sorted := make([]rerankResult, len(results))
	copy(sorted, results)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })

	indices := make([]int, len(sorted))
	for i, r := range sorted {
		indices[i] = r.Index
	}
	return indices, nil
}
