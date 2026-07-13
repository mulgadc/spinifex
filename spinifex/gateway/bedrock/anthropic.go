package gateway_bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	anthropicDefaultBaseURL   = "https://api.anthropic.com"
	anthropicMessagesPath     = "/v1/messages"
	anthropicAPIVersion       = "2023-06-01"
	anthropicDefaultMaxTokens = 4096
)

// anthropicModelIDPattern strips a "anthropic." vendor prefix and a trailing
// "-v<N>:<M>" Bedrock version suffix, e.g.
// "anthropic.claude-3-5-sonnet-20240620-v1:0" -> "claude-3-5-sonnet-20240620".
var anthropicModelIDPattern = regexp.MustCompile(`^anthropic\.(.+)-v\d+:\d+$`)

// anthropicModelID maps a Bedrock modelId to its Anthropic model name. IDs
// that don't match the expected shape pass through unchanged.
func anthropicModelID(bedrockModelID string) string {
	if m := anthropicModelIDPattern.FindStringSubmatch(bedrockModelID); m != nil {
		return m[1]
	}
	return bedrockModelID
}

// anthropicContentBlock is a single Messages API content block. Phase 1 is
// text-only, so only "text" blocks are produced or read.
type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int64              `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

// anthropicStopReasons maps Anthropic stop_reason values to Bedrock's
// StopReason enum. Unrecognised values (and tool/guardrail reasons Phase 1
// doesn't model) fall back to end_turn.
var anthropicStopReasons = map[string]string{
	"end_turn":      bedrockruntime.StopReasonEndTurn,
	"max_tokens":    bedrockruntime.StopReasonMaxTokens,
	"stop_sequence": bedrockruntime.StopReasonStopSequence,
	"tool_use":      bedrockruntime.StopReasonToolUse,
}

func mapAnthropicStopReason(reason string) string {
	if mapped, ok := anthropicStopReasons[reason]; ok {
		return mapped
	}
	return bedrockruntime.StopReasonEndTurn
}

// anthropicProvider calls the Anthropic Messages API directly. httpClient
// and baseURL are injectable for tests.
type anthropicProvider struct {
	httpClient *http.Client
	baseURL    string
}

func newAnthropicProviderClient() *anthropicProvider {
	return &anthropicProvider{
		httpClient: &http.Client{Timeout: providerHTTPTimeout},
		baseURL:    anthropicDefaultBaseURL,
	}
}

// anthropicBoundProvider adapts anthropicProvider to Provider by baking in a
// resolved per-account (or platform-default) API key.
type anthropicBoundProvider struct {
	inner  *anthropicProvider
	apiKey string
}

var _ Provider = (*anthropicBoundProvider)(nil)

func (b *anthropicBoundProvider) Converse(ctx context.Context, modelID string, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
	return b.inner.Converse(ctx, modelID, input, b.apiKey)
}

// newAnthropicProvider returns a Provider that calls the Anthropic Messages
// API with apiKey.
func newAnthropicProvider(apiKey string) Provider {
	return &anthropicBoundProvider{inner: newAnthropicProviderClient(), apiKey: apiKey}
}

// Converse translates input to an Anthropic Messages request, calls the API
// with apiKey, and translates the response back to a Bedrock ConverseOutput.
func (p *anthropicProvider) Converse(ctx context.Context, modelID string, input *bedrockruntime.ConverseInput, apiKey string) (*bedrockruntime.ConverseOutput, error) {
	reqBody, err := json.Marshal(buildAnthropicRequest(modelID, input))
	if err != nil {
		slog.Error("anthropic: failed to marshal request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+anthropicMessagesPath, bytes.NewReader(reqBody))
	if err != nil {
		slog.Error("anthropic: failed to build request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("X-Api-Key", apiKey)
	httpReq.Header.Set("Anthropic-Version", anthropicAPIVersion)
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := p.httpClient.Do(httpReq)
	latency := time.Since(start)
	if err != nil {
		slog.Error("anthropic: request failed", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("anthropic: failed to read response body", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("anthropic: upstream error", "model", modelID, "status", resp.StatusCode, "body", string(respBody))
		return nil, errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		slog.Error("anthropic: failed to parse response", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorModelErrorException)
	}

	return anthropicToConverseOutput(ar, latency), nil
}

// buildAnthropicRequest maps a Bedrock ConverseInput to an Anthropic Messages
// request. Non-text content blocks are dropped (Phase 1 is text-only).
func buildAnthropicRequest(modelID string, input *bedrockruntime.ConverseInput) anthropicRequest {
	maxTokens := int64(anthropicDefaultMaxTokens)
	var temperature, topP *float64
	var stopSequences []string
	if input.InferenceConfig != nil {
		if input.InferenceConfig.MaxTokens != nil {
			maxTokens = *input.InferenceConfig.MaxTokens
		}
		temperature = input.InferenceConfig.Temperature
		topP = input.InferenceConfig.TopP
		stopSequences = aws.StringValueSlice(input.InferenceConfig.StopSequences)
	}

	var systemParts []string
	for _, s := range input.System {
		if s != nil && s.Text != nil {
			systemParts = append(systemParts, *s.Text)
		}
	}

	messages := make([]anthropicMessage, 0, len(input.Messages))
	for _, m := range input.Messages {
		if m == nil {
			continue
		}
		var blocks []anthropicContentBlock
		for _, c := range m.Content {
			// TODO: Phase 1 is text-only; non-text content blocks are dropped.
			if c == nil || c.Text == nil {
				continue
			}
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: *c.Text})
		}
		messages = append(messages, anthropicMessage{Role: aws.StringValue(m.Role), Content: blocks})
	}

	return anthropicRequest{
		Model:         anthropicModelID(modelID),
		MaxTokens:     maxTokens,
		System:        strings.Join(systemParts, "\n"),
		Messages:      messages,
		Temperature:   temperature,
		TopP:          topP,
		StopSequences: stopSequences,
	}
}

// anthropicToConverseOutput maps an Anthropic Messages response to a Bedrock
// ConverseOutput, measuring latency from the caller-supplied wall-clock delta.
func anthropicToConverseOutput(ar anthropicResponse, latency time.Duration) *bedrockruntime.ConverseOutput {
	var content []*bedrockruntime.ContentBlock
	for _, block := range ar.Content {
		if block.Type != "text" {
			continue
		}
		content = append(content, &bedrockruntime.ContentBlock{Text: aws.String(block.Text)})
	}

	inputTokens := ar.Usage.InputTokens
	outputTokens := ar.Usage.OutputTokens

	return &bedrockruntime.ConverseOutput{
		Output: &bedrockruntime.ConverseOutput_{
			Message: &bedrockruntime.Message{
				Role:    aws.String(bedrockruntime.ConversationRoleAssistant),
				Content: content,
			},
		},
		StopReason: aws.String(mapAnthropicStopReason(ar.StopReason)),
		Usage: &bedrockruntime.TokenUsage{
			InputTokens:  aws.Int64(inputTokens),
			OutputTokens: aws.Int64(outputTokens),
			TotalTokens:  aws.Int64(inputTokens + outputTokens),
		},
		Metrics: &bedrockruntime.ConverseMetrics{
			LatencyMs: aws.Int64(latency.Milliseconds()),
		},
	}
}
