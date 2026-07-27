package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
	Model         string             `json:"model"`
	Prompt        string             `json:"prompt"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *vllmStreamOptions `json:"stream_options,omitempty"`
}

// llamaCompletionsStreamChunk is one OpenAI /v1/completions streaming SSE
// "data:" chunk.
type llamaCompletionsStreamChunk struct {
	Choices []llamaCompletionsStreamChoice `json:"choices"`
	Usage   *llamaCompletionsUsage         `json:"usage"`
}

type llamaCompletionsStreamChoice struct {
	Text         string  `json:"text"`
	FinishReason *string `json:"finish_reason"`
}

// llamaInvokeStreamChunk is a non-final Bedrock-native Llama
// invoke-with-response-stream chunk.
type llamaInvokeStreamChunk struct {
	Generation string `json:"generation"`
}

// llamaInvocationMetrics is Bedrock's per-invocation latency/token summary,
// carried on the final Llama invoke-with-response-stream chunk.
type llamaInvocationMetrics struct {
	InputTokenCount   int   `json:"inputTokenCount"`
	OutputTokenCount  int   `json:"outputTokenCount"`
	InvocationLatency int64 `json:"invocationLatency"`
	FirstByteLatency  int64 `json:"firstByteLatency"`
}

// llamaInvokeStreamFinalChunk is the final Bedrock-native Llama
// invoke-with-response-stream chunk, carrying stop/usage metadata.
type llamaInvokeStreamFinalChunk struct {
	Generation           string                 `json:"generation"`
	PromptTokenCount     int                    `json:"prompt_token_count"`
	GenerationTokenCount int                    `json:"generation_token_count"`
	StopReason           string                 `json:"stop_reason"`
	InvocationMetrics    llamaInvocationMetrics `json:"amazon-bedrock-invocationMetrics"`
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

var _ InvokeStreamAdapter = (*llamaInvokeAdapter)(nil)

// InvokeModelWithResponseStream resolves modelID's endpoint, opens a
// streaming OpenAI /v1/completions request (stream:true,
// stream_options.include_usage for the trailing usage chunk), and returns a
// source that translates each chunk to the Bedrock-native Llama streaming
// shape.
func (a *llamaInvokeAdapter) InvokeModelWithResponseStream(ctx context.Context, modelID string, body []byte) (invokeStreamSource, error) {
	baseURL, ok, err := a.endpointResolver.Endpoint(ctx, modelID)
	if err != nil {
		slog.Error("llama invoke-stream: endpoint resolution failed", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if !ok {
		return nil, errors.New(awserrors.ErrorModelNotReadyException)
	}

	var lr llamaInvokeRequest
	if err := json.Unmarshal(body, &lr); err != nil {
		slog.Error("llama invoke-stream: failed to parse request body", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorValidationException)
	}

	reqBody, err := json.Marshal(llamaCompletionsRequest{
		Model:         modelID,
		Prompt:        lr.Prompt,
		MaxTokens:     lr.MaxGenLen,
		Temperature:   lr.Temperature,
		TopP:          lr.TopP,
		Stream:        true,
		StreamOptions: &vllmStreamOptions{IncludeUsage: true},
	})
	if err != nil {
		slog.Error("llama invoke-stream: failed to marshal request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+llamaCompletionsPath, bytes.NewReader(reqBody)) //nolint:gosec // G704: baseURL is a resolved pinned self-host endpoint, not user input
	if err != nil {
		slog.Error("llama invoke-stream: failed to build request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := a.httpClient.Do(httpReq) //nolint:gosec // G704: httpReq targets the resolved pinned self-host endpoint, not user input
	if err != nil {
		slog.Error("llama invoke-stream: request failed", "model", modelID, "endpoint", baseURL, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		slog.Error("llama invoke-stream: upstream error", "model", modelID, "status", resp.StatusCode, "body", string(respBody))
		return nil, errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	return &llamaInvokeStreamSource{
		resp:    resp,
		scanner: newSSEScanner(resp.Body),
		start:   time.Now(),
	}, nil
}

// llamaInvokeStreamSource translates an OpenAI /v1/completions SSE stream
// into the Bedrock-native Llama invoke-with-response-stream chunk shape: a
// {"generation": "..."} chunk per text delta, and a final chunk carrying
// stop_reason, token counts, and amazon-bedrock-invocationMetrics. The final
// chunk's token counts arrive on vLLM's trailing usage-only chunk (via
// stream_options.include_usage), which follows the finish_reason chunk, so
// emitting the final chunk is deferred until that usage chunk (or EOF) is
// seen — otherwise the token counts would always be zero.
type llamaInvokeStreamSource struct {
	resp    *http.Response
	scanner *sseScanner
	start   time.Time

	gotFirstByte                   bool
	firstByte                      time.Time
	promptTokens, completionTokens int
	pendingFinal                   *llamaInvokeStreamFinalChunk
}

var _ invokeStreamSource = (*llamaInvokeStreamSource)(nil)

func (s *llamaInvokeStreamSource) Close() error {
	return s.resp.Body.Close()
}

func (s *llamaInvokeStreamSource) Next(_ context.Context) ([]byte, bool, error) {
	for {
		ev, ok, err := s.scanner.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if s.pendingFinal != nil {
					return s.emitFinal()
				}
				return nil, false, nil
			}
			return nil, false, newStreamFault(err)
		}
		if !ok {
			continue
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk llamaCompletionsStreamChunk
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			return nil, false, newStreamFault(fmt.Errorf("llama invoke-stream: decode chunk: %w", err))
		}

		if !s.gotFirstByte {
			s.gotFirstByte = true
			s.firstByte = time.Now()
		}
		if chunk.Usage != nil {
			s.promptTokens = chunk.Usage.PromptTokens
			s.completionTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			// The trailing usage-only chunk: emit the deferred final chunk
			// now that token counts are known.
			if s.pendingFinal != nil {
				return s.emitFinal()
			}
			continue
		}

		choice := chunk.Choices[0]
		if choice.FinishReason != nil {
			s.pendingFinal = &llamaInvokeStreamFinalChunk{
				Generation: choice.Text,
				StopReason: mapLlamaStopReason(*choice.FinishReason),
			}
			continue
		}

		out, err := json.Marshal(llamaInvokeStreamChunk{Generation: choice.Text})
		if err != nil {
			return nil, false, newStreamFault(fmt.Errorf("llama invoke-stream: marshal chunk: %w", err))
		}
		return out, true, nil
	}
}

// emitFinal fills in token counts/metrics on the deferred final chunk and
// marshals it.
func (s *llamaInvokeStreamSource) emitFinal() ([]byte, bool, error) {
	final := *s.pendingFinal
	s.pendingFinal = nil
	final.PromptTokenCount = s.promptTokens
	final.GenerationTokenCount = s.completionTokens
	final.InvocationMetrics = llamaInvocationMetrics{
		InputTokenCount:   s.promptTokens,
		OutputTokenCount:  s.completionTokens,
		InvocationLatency: time.Since(s.start).Milliseconds(),
		FirstByteLatency:  s.firstByte.Sub(s.start).Milliseconds(),
	}
	out, err := json.Marshal(final)
	if err != nil {
		return nil, false, newStreamFault(fmt.Errorf("llama invoke-stream: marshal final chunk: %w", err))
	}
	return out, true, nil
}
