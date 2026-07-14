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

// anthropicInvokeAdapter forwards Bedrock-native Claude InvokeModel bodies to
// the Anthropic Messages API almost as-is. httpClient and baseURL are
// injectable for tests.
type anthropicInvokeAdapter struct {
	httpClient *http.Client
	baseURL    string
}

func newAnthropicInvokeAdapterClient() *anthropicInvokeAdapter {
	return &anthropicInvokeAdapter{
		httpClient: &http.Client{Timeout: providerHTTPTimeout},
		baseURL:    anthropicDefaultBaseURL,
	}
}

// boundAnthropicInvokeAdapter adapts anthropicInvokeAdapter to InvokeAdapter
// by baking in a resolved per-account (or platform-default) API key.
type boundAnthropicInvokeAdapter struct {
	inner  *anthropicInvokeAdapter
	apiKey string
}

var _ InvokeAdapter = (*boundAnthropicInvokeAdapter)(nil)

func (b *boundAnthropicInvokeAdapter) InvokeModel(ctx context.Context, modelID string, body []byte) ([]byte, string, error) {
	return b.inner.InvokeModel(ctx, modelID, body, b.apiKey)
}

// newAnthropicInvokeAdapter returns an InvokeAdapter that forwards to the
// Anthropic Messages API with apiKey.
func newAnthropicInvokeAdapter(apiKey string) InvokeAdapter {
	return &boundAnthropicInvokeAdapter{inner: newAnthropicInvokeAdapterClient(), apiKey: apiKey}
}

// InvokeModel rewrites the Bedrock Claude InvokeModel body (drops the
// Bedrock-only anthropic_version field, injects model since Anthropic's API
// carries it in the body rather than the URL) and posts it to the Anthropic
// Messages API, returning the response verbatim.
func (a *anthropicInvokeAdapter) InvokeModel(ctx context.Context, modelID string, body []byte, apiKey string) ([]byte, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		slog.Error("anthropic invoke: failed to parse request body", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorValidationException)
	}
	delete(fields, "anthropic_version")

	modelJSON, err := json.Marshal(anthropicModelID(modelID))
	if err != nil {
		slog.Error("anthropic invoke: failed to marshal model id", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorInternalError)
	}
	fields["model"] = modelJSON

	reqBody, err := json.Marshal(fields)
	if err != nil {
		slog.Error("anthropic invoke: failed to marshal request", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+anthropicMessagesPath, bytes.NewReader(reqBody))
	if err != nil {
		slog.Error("anthropic invoke: failed to build request", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("X-Api-Key", apiKey)
	httpReq.Header.Set("Anthropic-Version", anthropicAPIVersion)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		slog.Error("anthropic invoke: request failed", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorServiceUnavailableException)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("anthropic invoke: failed to read response body", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("anthropic invoke: upstream error", "model", modelID, "status", resp.StatusCode, "body", string(respBody))
		return nil, "", errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	return respBody, "application/json", nil
}
