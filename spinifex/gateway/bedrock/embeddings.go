package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const embeddingsPath = "/v1/embeddings"

// DefaultEmbeddingModel is Ochre's CPU-served embedding model. Callers pin a
// dimension per vector index; the adapter itself never hardcodes one and
// returns whatever the endpoint yields.
const DefaultEmbeddingModel = "nomic-embed-text-v1.5"

// Embedder batch-embeds text against a self-hosted OpenAI/TEI-compatible
// endpoint, returning vectors ordered to match the caller's input slice.
type Embedder interface {
	Embed(ctx context.Context, modelID string, inputs []string) ([][]float32, error)
}

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embeddingsUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type embeddingsResponse struct {
	Data  []embeddingsDatum `json:"data"`
	Model string            `json:"model"`
	Usage embeddingsUsage   `json:"usage"`
}

// embeddingsProvider serves batched text embeddings over the OpenAI/TEI
// /v1/embeddings wire. httpClient and endpointResolver are injectable for
// tests, mirroring vllmProvider.
type embeddingsProvider struct {
	endpointResolver EndpointResolver
	httpClient       *http.Client
}

var _ Embedder = (*embeddingsProvider)(nil)

// newEmbeddingsProvider constructs an embeddingsProvider resolving endpoints
// via the shared EndpointResolver — the same static/registry-backed resolver
// vllmProvider uses, so the CPU embeddings endpoint never touches the
// tenant GPU serving lifecycle.
func newEmbeddingsProvider(endpointResolver EndpointResolver) *embeddingsProvider {
	return &embeddingsProvider{
		endpointResolver: endpointResolver,
		httpClient:       &http.Client{Timeout: providerHTTPTimeout},
	}
}

// NewEmbedder is newEmbeddingsProvider's exported constructor, for callers
// outside this package (e.g. the daemon's Ochre vector store wiring) that
// need an Embedder without reaching into gateway_bedrock's unexported types.
func NewEmbedder(endpointResolver EndpointResolver) Embedder {
	return newEmbeddingsProvider(endpointResolver)
}

// Embed resolves modelID's endpoint and batch-embeds inputs in a single
// request, returning vectors ordered to match inputs regardless of the order
// the endpoint's response lists them in. An empty inputs slice returns an
// empty result without calling the endpoint.
func (p *embeddingsProvider) Embed(ctx context.Context, modelID string, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	baseURL, ok, err := p.endpointResolver.Endpoint(ctx, modelID)
	if err != nil {
		slog.Error("embeddings: endpoint resolution failed", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if !ok {
		return nil, errors.New(awserrors.ErrorModelNotReadyException)
	}

	reqBody, err := json.Marshal(embeddingsRequest{Model: modelID, Input: inputs})
	if err != nil {
		slog.Error("embeddings: failed to marshal request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+embeddingsPath, bytes.NewReader(reqBody))
	if err != nil {
		slog.Error("embeddings: failed to build request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		slog.Error("embeddings: request failed", "model", modelID, "endpoint", baseURL, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("embeddings: failed to read response body", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("embeddings: upstream error", "model", modelID, "status", resp.StatusCode, "body", string(respBody))
		return nil, errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	var er embeddingsResponse
	if err := json.Unmarshal(respBody, &er); err != nil {
		slog.Error("embeddings: failed to parse response", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorModelErrorException)
	}

	vectors, err := orderEmbeddings(len(inputs), er.Data)
	if err != nil {
		slog.Error("embeddings: malformed response shape", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorModelErrorException)
	}
	return vectors, nil
}

// orderEmbeddings sorts a batch response's vectors back to the caller's
// input order by the endpoint-reported index, rather than assuming the
// endpoint preserves request order. A count mismatch, an out-of-range index,
// or a duplicate index is treated as a malformed response.
func orderEmbeddings(n int, data []embeddingsDatum) ([][]float32, error) {
	if len(data) != n {
		return nil, errors.New("embeddings count mismatch")
	}
	out := make([][]float32, n)
	for _, d := range data {
		if d.Index < 0 || d.Index >= n || out[d.Index] != nil {
			return nil, errors.New("embeddings index out of range or duplicated")
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}
