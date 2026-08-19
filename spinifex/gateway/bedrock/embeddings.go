package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	embeddingsPath = "/v1/embeddings"
	infoPath       = "/info"
	tokenizePath   = "/tokenize"

	// defaultMaxInputLength is the conservative token budget assumed when an
	// endpoint's TEI /info is unavailable or omits max_input_length -- bge-base's
	// 512-token cap, so a non-introspecting endpoint still guards chunk sizing.
	defaultMaxInputLength = 512

	// maxIntrospectionResponseBytes bounds the /info and /tokenize response
	// bodies read into memory, mirroring the ingest path's bounded reads.
	maxIntrospectionResponseBytes = 1 << 20
)

// DefaultEmbeddingModel is Ochre's GPU-served embedding model, co-resident in
// the demo bundle. Callers pin a dimension per vector index; the adapter
// itself never hardcodes one and returns whatever the endpoint yields.
const DefaultEmbeddingModel = "nomic-embed-text-v1.5"

// Embedder batch-embeds text against a self-hosted OpenAI/TEI-compatible
// endpoint, returning vectors ordered to match the caller's input slice.
type Embedder interface {
	Embed(ctx context.Context, modelID string, inputs []string) ([][]float32, error)
}

// TokenLimiter is implemented by Embedders that can report the served
// model's real token budget, so callers (the ingest chunker) can size chunks
// to it instead of a fixed rune guess. It is optional -- a caller type-
// asserts for it rather than requiring it on Embedder, so a non-TEI or test
// double Embedder still satisfies Embedder alone.
type TokenLimiter interface {
	// MaxInputLength returns modelID's served max_input_length in tokens,
	// querying and caching TEI GET /info once per resolved base URL. It
	// never errors: an unreachable or non-TEI endpoint logs at debug and
	// returns defaultMaxInputLength, since introspection is best-effort and
	// must never block ingestion.
	MaxInputLength(ctx context.Context, modelID string) int
	// CountTokens returns text's exact token count via TEI POST /tokenize.
	// ok is false when the endpoint doesn't support /tokenize or the call
	// fails, telling the caller to fall back to its own estimate.
	CountTokens(ctx context.Context, modelID, text string) (count int, ok bool)
}

// infoResponse is the subset of TEI's GET /info payload this adapter reads;
// every other field (model id, dtype, etc.) is ignored.
type infoResponse struct {
	MaxInputLength int `json:"max_input_length"`
}

// tokenizeToken is one element of TEI's POST /tokenize response array --
// only the array's length (the token count) is used here.
type tokenizeToken struct {
	ID int `json:"id"`
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
	// infoCache holds baseURL -> max_input_length (int), populated once per
	// endpoint the first time MaxInputLength resolves it. TEI /info never
	// changes for a running server, so this is cached for the process
	// lifetime rather than re-fetched per call.
	infoCache sync.Map
}

var _ Embedder = (*embeddingsProvider)(nil)
var _ TokenLimiter = (*embeddingsProvider)(nil)

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

// maxEmbedClientBatch bounds inputs per embeddings request to TEI's default
// max_client_batch_size, so a document with more chunks is split across
// requests rather than rejected with a 422.
const maxEmbedClientBatch = 32

// Embed resolves modelID's endpoint once and batch-embeds inputs, splitting
// them into requests of at most maxEmbedClientBatch so a large document does
// not exceed the endpoint's client batch limit. Vectors are returned ordered
// to match inputs; an empty inputs slice returns an empty result.
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

	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += maxEmbedClientBatch {
		end := min(start+maxEmbedClientBatch, len(inputs))
		vectors, err := p.embedBatch(ctx, modelID, baseURL, inputs[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

// embedBatch embeds one already-bounded slice of inputs against baseURL in a
// single request, ordering the response vectors back to inputs' order
// regardless of the order the endpoint lists them in.
func (p *embeddingsProvider) embedBatch(ctx context.Context, modelID, baseURL string, inputs []string) ([][]float32, error) {
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

// MaxInputLength implements TokenLimiter: it resolves modelID's endpoint and
// returns its cached (or freshly fetched) TEI /info max_input_length,
// falling back to defaultMaxInputLength whenever the endpoint can't be
// resolved or /info can't be read -- introspection is best-effort and must
// never fail ingestion.
func (p *embeddingsProvider) MaxInputLength(ctx context.Context, modelID string) int {
	baseURL, ok, err := p.endpointResolver.Endpoint(ctx, modelID)
	if err != nil || !ok {
		return defaultMaxInputLength
	}
	if cached, ok := p.infoCache.Load(baseURL); ok {
		if limit, ok := cached.(int); ok {
			return limit
		}
	}
	limit := p.fetchMaxInputLength(ctx, baseURL)
	p.infoCache.Store(baseURL, limit)
	return limit
}

// fetchMaxInputLength queries baseURL's TEI GET /info once. Any failure --
// unreachable endpoint, non-2xx status, or a response missing/zeroing
// max_input_length -- logs at debug and returns defaultMaxInputLength rather
// than propagating an error, since a non-TEI endpoint (no /info at all) is a
// normal, supported configuration, not a fault.
func (p *embeddingsProvider) fetchMaxInputLength(ctx context.Context, baseURL string) int {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+infoPath, nil)
	if err != nil {
		slog.Debug("embeddings: failed to build /info request, using default max_input_length", "endpoint", baseURL, "default", defaultMaxInputLength, "err", err)
		return defaultMaxInputLength
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		slog.Debug("embeddings: /info request failed, using default max_input_length", "endpoint", baseURL, "default", defaultMaxInputLength, "err", err)
		return defaultMaxInputLength
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Debug("embeddings: /info returned non-OK status, using default max_input_length", "endpoint", baseURL, "status", resp.StatusCode, "default", defaultMaxInputLength)
		return defaultMaxInputLength
	}

	var info infoResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxIntrospectionResponseBytes)).Decode(&info); err != nil || info.MaxInputLength <= 0 {
		slog.Debug("embeddings: /info missing or invalid max_input_length, using default", "endpoint", baseURL, "default", defaultMaxInputLength)
		return defaultMaxInputLength
	}
	return info.MaxInputLength
}

// CountTokens implements TokenLimiter: it POSTs text to baseURL's TEI
// /tokenize and returns the response array's length as the exact token
// count. Any failure returns ok=false rather than a guessed count, so the
// caller falls back to its own conservative estimate instead of trusting a
// wrong one.
func (p *embeddingsProvider) CountTokens(ctx context.Context, modelID, text string) (int, bool) {
	baseURL, ok, err := p.endpointResolver.Endpoint(ctx, modelID)
	if err != nil || !ok {
		return 0, false
	}

	reqBody, err := json.Marshal(map[string]string{"inputs": text})
	if err != nil {
		return 0, false
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+tokenizePath, bytes.NewReader(reqBody))
	if err != nil {
		return 0, false
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		slog.Debug("embeddings: /tokenize request failed", "endpoint", baseURL, "err", err)
		return 0, false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Debug("embeddings: /tokenize returned non-OK status", "endpoint", baseURL, "status", resp.StatusCode)
		return 0, false
	}

	var tokens []tokenizeToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxIntrospectionResponseBytes)).Decode(&tokens); err != nil {
		slog.Debug("embeddings: /tokenize response decode failed", "endpoint", baseURL, "err", err)
		return 0, false
	}
	return len(tokens), true
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
