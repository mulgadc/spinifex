package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const vllmChatCompletionsPath = "/v1/chat/completions"

// EndpointResolver resolves a self-hosted model's OpenAI-compatible base URL.
// A later task backs this with the daemon's endpoint registry.
type EndpointResolver interface {
	Endpoint(ctx context.Context, modelID string) (baseURL string, ok bool, err error)
}

// staticEndpointResolver is a fixed modelID->baseURL map, for tests and
// config-driven single-endpoint setups.
type staticEndpointResolver map[string]string

var _ EndpointResolver = staticEndpointResolver(nil)

// NewStaticEndpointResolver returns an EndpointResolver backed by a fixed
// modelID->baseURL map. A nil or empty map resolves nothing.
func NewStaticEndpointResolver(endpoints map[string]string) EndpointResolver {
	return staticEndpointResolver(endpoints)
}

func (s staticEndpointResolver) Endpoint(_ context.Context, modelID string) (string, bool, error) {
	baseURL, ok := s[modelID]
	return baseURL, ok, nil
}

type vllmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type vllmChatRequest struct {
	Model       string        `json:"model"`
	Messages    []vllmMessage `json:"messages"`
	MaxTokens   *int64        `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
}

type vllmChoice struct {
	Message      vllmMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type vllmUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type vllmChatResponse struct {
	Choices []vllmChoice `json:"choices"`
	Usage   vllmUsage    `json:"usage"`
}

// vllmFinishReasons maps OpenAI finish_reason values to Bedrock's StopReason
// enum. Unrecognised values fall back to end_turn.
var vllmFinishReasons = map[string]string{
	"stop":   bedrockruntime.StopReasonEndTurn,
	"length": bedrockruntime.StopReasonMaxTokens,
}

func mapVLLMFinishReason(reason string) string {
	if mapped, ok := vllmFinishReasons[reason]; ok {
		return mapped
	}
	return bedrockruntime.StopReasonEndTurn
}

// vllmProvider serves self-hosted open-weight models over the OpenAI
// chat-completions wire. httpClient and endpointResolver are injectable for
// tests.
type vllmProvider struct {
	endpointResolver EndpointResolver
	httpClient       *http.Client
}

var _ Provider = (*vllmProvider)(nil)

func newVLLMProvider(endpointResolver EndpointResolver) *vllmProvider {
	return &vllmProvider{
		endpointResolver: endpointResolver,
		httpClient:       &http.Client{Timeout: providerHTTPTimeout},
	}
}

// Converse resolves modelID's endpoint, translates input to an OpenAI
// chat-completions request, calls the endpoint, and translates the response
// back to a Bedrock ConverseOutput.
func (p *vllmProvider) Converse(ctx context.Context, modelID string, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
	baseURL, ok, err := p.endpointResolver.Endpoint(ctx, modelID)
	if err != nil {
		slog.Error("vllm: endpoint resolution failed", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if !ok {
		return nil, errors.New(awserrors.ErrorModelNotReadyException)
	}

	reqBody, err := json.Marshal(buildVLLMRequest(modelID, input))
	if err != nil {
		slog.Error("vllm: failed to marshal request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+vllmChatCompletionsPath, bytes.NewReader(reqBody))
	if err != nil {
		slog.Error("vllm: failed to build request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := p.httpClient.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		slog.Error("vllm: request failed", "model", modelID, "endpoint", baseURL, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("vllm: failed to read response body", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("vllm: upstream error", "model", modelID, "status", resp.StatusCode, "body", string(respBody))
		return nil, errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	var cr vllmChatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		slog.Error("vllm: failed to parse response", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorModelErrorException)
	}

	return vllmToConverseOutput(cr, latency)
}

// buildVLLMRequest maps a Bedrock ConverseInput to an OpenAI chat-completions
// request. Non-text content blocks are dropped (Phase 1 is text-only).
func buildVLLMRequest(modelID string, input *bedrockruntime.ConverseInput) vllmChatRequest {
	var messages []vllmMessage

	var systemParts []string
	for _, s := range input.System {
		if s != nil && s.Text != nil {
			systemParts = append(systemParts, *s.Text)
		}
	}
	if len(systemParts) > 0 {
		messages = append(messages, vllmMessage{Role: "system", Content: strings.Join(systemParts, "\n")})
	}

	for _, m := range input.Messages {
		if m == nil {
			continue
		}
		var textParts []string
		for _, c := range m.Content {
			// TODO: Phase 1 is text-only; non-text content blocks are dropped.
			if c == nil || c.Text == nil {
				continue
			}
			textParts = append(textParts, *c.Text)
		}
		messages = append(messages, vllmMessage{Role: aws.StringValue(m.Role), Content: strings.Join(textParts, "\n")})
	}

	req := vllmChatRequest{Model: modelID, Messages: messages}
	if input.InferenceConfig != nil {
		req.MaxTokens = input.InferenceConfig.MaxTokens
		req.Temperature = input.InferenceConfig.Temperature
		req.TopP = input.InferenceConfig.TopP
		req.Stop = aws.StringValueSlice(input.InferenceConfig.StopSequences)
	}
	return req
}

// vllmToConverseOutput maps an OpenAI chat-completions response to a Bedrock
// ConverseOutput, measuring latency from the caller-supplied wall-clock delta.
func vllmToConverseOutput(cr vllmChatResponse, latency time.Duration) (*bedrockruntime.ConverseOutput, error) {
	if len(cr.Choices) == 0 {
		return nil, errors.New(awserrors.ErrorModelErrorException)
	}
	choice := cr.Choices[0]
	inputTokens := cr.Usage.PromptTokens
	outputTokens := cr.Usage.CompletionTokens

	return &bedrockruntime.ConverseOutput{
		Output: &bedrockruntime.ConverseOutput_{
			Message: &bedrockruntime.Message{
				Role:    aws.String(bedrockruntime.ConversationRoleAssistant),
				Content: []*bedrockruntime.ContentBlock{{Text: aws.String(choice.Message.Content)}},
			},
		},
		StopReason: aws.String(mapVLLMFinishReason(choice.FinishReason)),
		Usage: &bedrockruntime.TokenUsage{
			InputTokens:  aws.Int64(inputTokens),
			OutputTokens: aws.Int64(outputTokens),
			TotalTokens:  aws.Int64(inputTokens + outputTokens),
		},
		Metrics: &bedrockruntime.ConverseMetrics{LatencyMs: aws.Int64(latency.Milliseconds())},
	}, nil
}
