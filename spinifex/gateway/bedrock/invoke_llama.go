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

const llamaCompletionsPath = "/v1/completions"

// llamaInvokeRequest is the Bedrock-native Llama InvokeModel request body.
type llamaInvokeRequest struct {
	Prompt      string   `json:"prompt"`
	MaxGenLen   int      `json:"max_gen_len,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

// llamaInvokeResponse is the Bedrock-native Llama InvokeModel response body.
type llamaInvokeResponse struct {
	Generation           string `json:"generation"`
	PromptTokenCount     int    `json:"prompt_token_count"`
	GenerationTokenCount int    `json:"generation_token_count"`
	StopReason           string `json:"stop_reason"`
}

// llamaCompletionsRequest is the OpenAI /v1/completions request vLLM serves.
type llamaCompletionsRequest struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

type llamaCompletionsChoice struct {
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

type llamaCompletionsUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type llamaCompletionsResponse struct {
	Choices []llamaCompletionsChoice `json:"choices"`
	Usage   llamaCompletionsUsage    `json:"usage"`
}

// llamaFinishReasons maps OpenAI finish_reason values to Bedrock Llama's
// stop_reason values. Unrecognised values fall back to "stop".
var llamaFinishReasons = map[string]string{
	"stop":   "stop",
	"length": "length",
}

func mapLlamaStopReason(reason string) string {
	if mapped, ok := llamaFinishReasons[reason]; ok {
		return mapped
	}
	return "stop"
}

// llamaInvokeAdapter serves Bedrock-native Llama InvokeModel requests over a
// self-hosted vLLM OpenAI-completions endpoint. httpClient and
// endpointResolver are injectable for tests.
type llamaInvokeAdapter struct {
	endpointResolver EndpointResolver
	httpClient       *http.Client
}

var _ InvokeAdapter = (*llamaInvokeAdapter)(nil)

func newLlamaInvokeAdapter(endpointResolver EndpointResolver) *llamaInvokeAdapter {
	return &llamaInvokeAdapter{
		endpointResolver: endpointResolver,
		httpClient:       &http.Client{Timeout: providerHTTPTimeout},
	}
}

// InvokeModel resolves modelID's endpoint, translates the Bedrock-native
// Llama request to an OpenAI completions request, calls the endpoint, and
// translates the response back to the Bedrock Llama shape.
func (a *llamaInvokeAdapter) InvokeModel(ctx context.Context, modelID string, body []byte) ([]byte, string, error) {
	baseURL, ok, err := a.endpointResolver.Endpoint(ctx, modelID)
	if err != nil {
		slog.Error("llama invoke: endpoint resolution failed", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if !ok {
		return nil, "", errors.New(awserrors.ErrorModelNotReadyException)
	}

	var lr llamaInvokeRequest
	if err := json.Unmarshal(body, &lr); err != nil {
		slog.Error("llama invoke: failed to parse request body", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorValidationException)
	}

	reqBody, err := json.Marshal(llamaCompletionsRequest{
		Model:       modelID,
		Prompt:      lr.Prompt,
		MaxTokens:   lr.MaxGenLen,
		Temperature: lr.Temperature,
		TopP:        lr.TopP,
	})
	if err != nil {
		slog.Error("llama invoke: failed to marshal request", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+llamaCompletionsPath, bytes.NewReader(reqBody))
	if err != nil {
		slog.Error("llama invoke: failed to build request", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		slog.Error("llama invoke: request failed", "model", modelID, "endpoint", baseURL, "err", err)
		return nil, "", errors.New(awserrors.ErrorServiceUnavailableException)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("llama invoke: failed to read response body", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("llama invoke: upstream error", "model", modelID, "status", resp.StatusCode, "body", string(respBody))
		return nil, "", errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	var cr llamaCompletionsResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		slog.Error("llama invoke: failed to parse response", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorModelErrorException)
	}
	if len(cr.Choices) == 0 {
		return nil, "", errors.New(awserrors.ErrorModelErrorException)
	}
	choice := cr.Choices[0]

	out, err := json.Marshal(llamaInvokeResponse{
		Generation:           choice.Text,
		PromptTokenCount:     cr.Usage.PromptTokens,
		GenerationTokenCount: cr.Usage.CompletionTokens,
		StopReason:           mapLlamaStopReason(choice.FinishReason),
	})
	if err != nil {
		slog.Error("llama invoke: failed to marshal response", "model", modelID, "err", err)
		return nil, "", errors.New(awserrors.ErrorInternalError)
	}

	return out, "application/json", nil
}
