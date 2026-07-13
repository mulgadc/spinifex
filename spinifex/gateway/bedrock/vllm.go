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
	Model         string             `json:"model"`
	Messages      []vllmMessage      `json:"messages"`
	MaxTokens     *int64             `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	Stop          []string           `json:"stop,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *vllmStreamOptions `json:"stream_options,omitempty"`
}

// vllmStreamOptions requests the trailing usage-only chunk vLLM/OpenAI
// otherwise omits from a streaming response.
type vllmStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// vllmStreamChunk is one OpenAI chat-completions streaming SSE "data:" chunk.
type vllmStreamChunk struct {
	Choices []vllmStreamChoice `json:"choices"`
	Usage   *vllmUsage         `json:"usage"`
}

type vllmStreamChoice struct {
	Delta        vllmMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
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

var _ ConverseStreamProvider = (*vllmProvider)(nil)

// ConverseStream resolves modelID's endpoint, opens a streaming OpenAI
// chat-completions request (stream:true, stream_options.include_usage for
// the trailing usage chunk), and returns a converseStreamSource that
// reframes the SSE into the normalized Bedrock ConverseStream taxonomy.
func (p *vllmProvider) ConverseStream(ctx context.Context, modelID string, input *bedrockruntime.ConverseStreamInput) (converseStreamSource, error) {
	baseURL, ok, err := p.endpointResolver.Endpoint(ctx, modelID)
	if err != nil {
		slog.Error("vllm stream: endpoint resolution failed", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if !ok {
		return nil, errors.New(awserrors.ErrorModelNotReadyException)
	}

	req := buildVLLMRequest(modelID, converseStreamToConverseInput(input))
	req.Stream = true
	req.StreamOptions = &vllmStreamOptions{IncludeUsage: true}

	reqBody, err := json.Marshal(req)
	if err != nil {
		slog.Error("vllm stream: failed to marshal request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+vllmChatCompletionsPath, bytes.NewReader(reqBody)) //nolint:gosec // G704: baseURL is a resolved pinned self-host endpoint, not user input
	if err != nil {
		slog.Error("vllm stream: failed to build request", "model", modelID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.httpClient.Do(httpReq) //nolint:gosec // G704: httpReq targets the resolved pinned self-host endpoint, not user input
	if err != nil {
		slog.Error("vllm stream: request failed", "model", modelID, "endpoint", baseURL, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailableException)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		slog.Error("vllm stream: upstream error", "model", modelID, "status", resp.StatusCode, "body", string(respBody))
		return nil, errors.New(mapUpstreamStatus(resp.StatusCode))
	}

	return &vllmConverseStreamSource{
		resp:    resp,
		scanner: newSSEScanner(resp.Body),
		start:   time.Now(),
	}, nil
}

// vllmConverseStreamSource reframes a vLLM (OpenAI chat-completions) SSE
// stream into the normalized Bedrock ConverseStream event taxonomy:
// messageStart+contentBlockStart on the first chunk, contentBlockDelta per
// content delta, contentBlockStop+messageStop on finish_reason, metadata on
// the trailing usage-only chunk (or defensively at EOF if the upstream never
// sends one). Events are queued because a single upstream chunk can fan out
// to more than one normalized event.
type vllmConverseStreamSource struct {
	resp    *http.Response
	scanner *sseScanner
	start   time.Time

	queue           []ConverseStreamEvent
	started         bool
	stopped         bool
	metadataEmitted bool
	inputTokens     int64
	outputTokens    int64
}

var _ converseStreamSource = (*vllmConverseStreamSource)(nil)

func (s *vllmConverseStreamSource) Close() error {
	return s.resp.Body.Close()
}

func (s *vllmConverseStreamSource) Next(_ context.Context) (ConverseStreamEvent, bool, error) {
	for len(s.queue) == 0 {
		ev, ok, err := s.scanner.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.closeOut(bedrockruntime.StopReasonEndTurn)
				if !s.metadataEmitted {
					s.emitMetadata()
				}
				break
			}
			return ConverseStreamEvent{}, false, newStreamFault(err)
		}
		if !ok {
			continue
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk vllmStreamChunk
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			return ConverseStreamEvent{}, false, newStreamFault(fmt.Errorf("vllm stream: decode chunk: %w", err))
		}
		s.consume(chunk)
	}

	if len(s.queue) == 0 {
		return ConverseStreamEvent{}, false, nil
	}
	next := s.queue[0]
	s.queue = s.queue[1:]
	return next, true, nil
}

// consume folds one decoded chat-completion chunk into the queued normalized
// events and running usage totals.
func (s *vllmConverseStreamSource) consume(chunk vllmStreamChunk) {
	if chunk.Usage != nil {
		s.inputTokens = chunk.Usage.PromptTokens
		s.outputTokens = chunk.Usage.CompletionTokens
	}
	if len(chunk.Choices) == 0 {
		// The trailing usage-only chunk from stream_options.include_usage.
		if chunk.Usage != nil {
			s.emitMetadata()
		}
		return
	}

	choice := chunk.Choices[0]
	if !s.started {
		s.started = true
		s.queue = append(s.queue,
			ConverseStreamEvent{
				Kind:         converseStreamEventMessageStart,
				MessageStart: &bedrockruntime.MessageStartEvent{Role: aws.String(bedrockruntime.ConversationRoleAssistant)},
			},
			ConverseStreamEvent{
				Kind:              converseStreamEventContentBlockStart,
				ContentBlockStart: &bedrockruntime.ContentBlockStartEvent{ContentBlockIndex: aws.Int64(0), Start: &bedrockruntime.ContentBlockStart{}},
			},
		)
	}

	if choice.Delta.Content != "" {
		s.queue = append(s.queue, ConverseStreamEvent{
			Kind: converseStreamEventContentBlockDelta,
			ContentBlockDelta: &bedrockruntime.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int64(0),
				Delta:             &bedrockruntime.ContentBlockDelta{Text: aws.String(choice.Delta.Content)},
			},
		})
	}

	if choice.FinishReason != nil {
		s.closeOut(mapVLLMFinishReason(*choice.FinishReason))
	}
}

// closeOut queues contentBlockStop+messageStop exactly once.
func (s *vllmConverseStreamSource) closeOut(stopReason string) {
	if s.stopped {
		return
	}
	s.stopped = true
	s.queue = append(s.queue,
		ConverseStreamEvent{
			Kind:             converseStreamEventContentBlockStop,
			ContentBlockStop: &bedrockruntime.ContentBlockStopEvent{ContentBlockIndex: aws.Int64(0)},
		},
		ConverseStreamEvent{
			Kind:        converseStreamEventMessageStop,
			MessageStop: &bedrockruntime.MessageStopEvent{StopReason: aws.String(stopReason)},
		},
	)
}

// emitMetadata queues the trailing metadata event exactly once.
func (s *vllmConverseStreamSource) emitMetadata() {
	if s.metadataEmitted {
		return
	}
	s.metadataEmitted = true
	s.queue = append(s.queue, ConverseStreamEvent{
		Kind: converseStreamEventMetadata,
		Metadata: &bedrockruntime.ConverseStreamMetadataEvent{
			Usage: &bedrockruntime.TokenUsage{
				InputTokens:  aws.Int64(s.inputTokens),
				OutputTokens: aws.Int64(s.outputTokens),
				TotalTokens:  aws.Int64(s.inputTokens + s.outputTokens),
			},
			Metrics: &bedrockruntime.ConverseStreamMetrics{LatencyMs: aws.Int64(time.Since(s.start).Milliseconds())},
		},
	})
}
