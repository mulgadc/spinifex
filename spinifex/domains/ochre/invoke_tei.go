package ochre

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// teiEmbeddingsPath is TEI's OpenAI-compatible embeddings route, whose
// request and response bodies are this model-specific InvokeModel contract.
const teiEmbeddingsPath = "/v1/embeddings"

// teiEmbeddingsResponseLimit caps the upstream body read: 32 inputs of a
// 768-dim float vector stay well under it.
const teiEmbeddingsResponseLimit = 16 << 20

// teiEmbeddingsRequest is the accepted InvokeModel body. Dimensions must be
// null: TEI truncates without the model's own Matryoshka normalisation.
type teiEmbeddingsRequest struct {
	Input          json.RawMessage `json:"input"`
	Model          json.RawMessage `json:"model,omitempty"`
	EncodingFormat json.RawMessage `json:"encoding_format,omitempty"`
	Dimensions     json.RawMessage `json:"dimensions,omitempty"`
}

// teiEmbeddingsUpstreamRequest is what is forwarded to TEI, without model.
type teiEmbeddingsUpstreamRequest struct {
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format"`
}

// teiEmbeddingsDatum holds coordinates as pointers, because encoding/json
// decodes a null element as 0.
type teiEmbeddingsDatum struct {
	Object    string     `json:"object"`
	Embedding []*float64 `json:"embedding"`
	Index     *int       `json:"index"`
}

type teiEmbeddingsUsage struct {
	PromptTokens *int64 `json:"prompt_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
}

// teiEmbeddingsResponse is TEI's response and, with model rewritten to the
// catalog ID, the InvokeModel response body.
type teiEmbeddingsResponse struct {
	Object string               `json:"object"`
	Data   []teiEmbeddingsDatum `json:"data"`
	Model  string               `json:"model"`
	Usage  *teiEmbeddingsUsage  `json:"usage"`
}

// teiEmbeddingInvokeAdapter serves embedding InvokeModel requests from a
// static or operator-pinned TEI endpoint only; it never starts one.
type teiEmbeddingInvokeAdapter struct {
	endpointResolver EndpointResolver
	httpClient       *http.Client
}

var _ InvokeAdapter = (*teiEmbeddingInvokeAdapter)(nil)

func newTEIEmbeddingInvokeAdapter(endpointResolver EndpointResolver) *teiEmbeddingInvokeAdapter {
	return &teiEmbeddingInvokeAdapter{
		endpointResolver: endpointResolver,
		httpClient: &http.Client{
			Timeout: providerHTTPTimeout,
			// A redirect would resend the embedding input to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// parseTEIEmbeddingsRequest validates body against the contract and returns
// the upstream request and its input count.
func parseTEIEmbeddingsRequest(modelID string, body []byte) ([]byte, int, error) {
	invalid := errors.New(awserrors.ErrorValidationException)
	// Valid first: the decoder alone accepts trailing bytes after one value.
	if !json.Valid(body) {
		return nil, 0, invalid
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req teiEmbeddingsRequest
	if err := dec.Decode(&req); err != nil {
		return nil, 0, invalid
	}
	if !optionalStringIs(req.Model, modelID) || !optionalStringIs(req.EncodingFormat, "float") {
		return nil, 0, invalid
	}
	if len(req.Dimensions) > 0 && !bytes.Equal(bytes.TrimSpace(req.Dimensions), []byte("null")) {
		return nil, 0, invalid
	}
	n, ok := countEmbeddingInputs(req.Input)
	if !ok {
		return nil, 0, invalid
	}
	upstream, err := json.Marshal(teiEmbeddingsUpstreamRequest{Input: req.Input, EncodingFormat: "float"})
	if err != nil {
		return nil, 0, errors.New(awserrors.ErrorInternalError)
	}
	return upstream, n, nil
}

// optionalStringIs reports whether raw was omitted or is the JSON string want.
// An explicit null is neither, so it is refused rather than read as omission.
func optionalStringIs(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return true
	}
	var got string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &got) != nil {
		return false
	}
	return got == want
}

// countEmbeddingInputs accepts a nonempty string or 1..teiMaxClientBatchSize
// nonempty strings.
func countEmbeddingInputs(raw json.RawMessage) (int, bool) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return 1, single != ""
	}
	var batch []string
	if err := json.Unmarshal(raw, &batch); err != nil || len(batch) == 0 || len(batch) > teiMaxClientBatchSize {
		return 0, false
	}
	if slices.Contains(batch, "") {
		return 0, false
	}
	return len(batch), true
}

// InvokeModel validates the body before any endpoint lookup, then forwards it
// to TEI and returns its response with model set to modelID.
func (a *teiEmbeddingInvokeAdapter) InvokeModel(ctx context.Context, modelID string, body []byte) ([]byte, string, error) {
	upstream, n, err := parseTEIEmbeddingsRequest(modelID, body)
	if err != nil {
		return nil, "", err
	}

	pinned, ok := a.endpointResolver.(PinnedEndpointResolver)
	if !ok {
		return nil, "", errors.New(awserrors.ErrorModelNotReadyException)
	}
	baseURL, ok, err := pinned.PinnedEndpoint(ctx, modelID)
	if err != nil {
		slog.Error("tei invoke: endpoint lookup failed", "model", modelID, "err", err)
		return nil, "", resolveEndpointError(err)
	}
	if !ok {
		return nil, "", errors.New(awserrors.ErrorModelNotReadyException)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+teiEmbeddingsPath, bytes.NewReader(upstream))
	if err != nil {
		slog.Error("tei invoke: failed to build request", "model", modelID, "endpoint", endpointLogID(baseURL))
		return nil, "", errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		// Neither the URL nor err is logged: a static URL may carry userinfo.
		slog.Error("tei invoke: request failed", "model", modelID, "endpoint", endpointLogID(baseURL),
			"failure", transportFailureClass(err))
		return nil, "", errors.New(awserrors.ErrorServiceUnavailableException)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, teiEmbeddingsResponseLimit+1))
	if err != nil {
		slog.Error("tei invoke: failed to read response body", "model", modelID, "endpoint", endpointLogID(baseURL),
			"failure", transportFailureClass(err))
		return nil, "", errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body is not logged: TEI validation errors can quote the input.
		slog.Error("tei invoke: upstream error", "model", modelID, "status", resp.StatusCode)
		return nil, "", errors.New(mapTEIStatus(resp.StatusCode))
	}
	if len(respBody) > teiEmbeddingsResponseLimit {
		slog.Error("tei invoke: response exceeds limit", "model", modelID)
		return nil, "", errors.New(awserrors.ErrorModelErrorException)
	}

	entry, _ := lookupCatalogEntry(modelID)
	out, err := normalizeTEIEmbeddingsResponse(modelID, respBody, n, entry.EmbeddingDimensions)
	if err != nil {
		slog.Error("tei invoke: malformed response", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorModelErrorException)
	}
	return out, "application/json", nil
}

// endpointLogID names baseURL by scheme and host only, dropping any userinfo,
// path or query.
func endpointLogID(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "unparseable"
	}
	return u.Scheme + "://" + u.Host
}

// transportFailureClass reduces a transport error to a fixed label, since
// its text can quote the request URL.
func transportFailureClass(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "unreachable"
	}
}

// mapTEIStatus maps TEI's input rejections (413 payload too large, 422
// validation) to ValidationException and defers to mapUpstreamStatus otherwise.
func mapTEIStatus(status int) string {
	switch status {
	case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return awserrors.ErrorValidationException
	default:
		return mapUpstreamStatus(status)
	}
}

// normalizeTEIEmbeddingsResponse requires n vectors of dims numbers indexed
// 0..n-1 and nonnegative token counts, and replaces model with modelID.
func normalizeTEIEmbeddingsResponse(modelID string, body []byte, n, dims int) ([]byte, error) {
	var r teiEmbeddingsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if len(r.Data) != n {
		return nil, errors.New("vector count does not match the input count")
	}
	seen := make([]bool, n)
	for _, d := range r.Data {
		if d.Index == nil || *d.Index < 0 || *d.Index >= n || seen[*d.Index] {
			return nil, errors.New("missing, duplicate or out-of-range vector")
		}
		if dims <= 0 || len(d.Embedding) != dims {
			return nil, errors.New("vector width differs from the model's native width")
		}
		if slices.Contains(d.Embedding, nil) {
			return nil, errors.New("null vector element")
		}
		seen[*d.Index] = true
	}
	if r.Usage == nil || r.Usage.PromptTokens == nil || *r.Usage.PromptTokens < 0 ||
		r.Usage.TotalTokens == nil || *r.Usage.TotalTokens < 0 {
		return nil, errors.New("missing or negative token usage")
	}
	r.Object = "list"
	r.Model = modelID
	return json.Marshal(r)
}

// embeddingUsage reads the prompt token count from a normalized embedding
// response; ok is false for anything else.
func embeddingUsage(respBody []byte) (inputTokens int64, ok bool) {
	var r teiEmbeddingsResponse
	if json.Unmarshal(respBody, &r) != nil || r.Usage == nil || r.Usage.PromptTokens == nil {
		return 0, false
	}
	return *r.Usage.PromptTokens, true
}
