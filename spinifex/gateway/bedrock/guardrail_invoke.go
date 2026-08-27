package gateway_bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// extractLlamaPromptTexts parses a Bedrock-native Llama InvokeModel request
// body into its single INPUT-checkable text, mirroring extractTokenUsage's
// reuse of the family's own wire struct.
func extractLlamaPromptTexts(body []byte) ([]string, bool) {
	var lr llamaInvokeRequest
	if json.Unmarshal(body, &lr) != nil {
		return nil, false
	}
	if lr.Prompt == "" {
		return nil, true
	}
	return []string{lr.Prompt}, true
}

// extractLlamaCompletionTexts parses a Bedrock-native Llama InvokeModel
// response body into its single OUTPUT-checkable text.
func extractLlamaCompletionTexts(body []byte) ([]string, bool) {
	var lr llamaInvokeResponse
	if json.Unmarshal(body, &lr) != nil {
		return nil, false
	}
	return []string{lr.Generation}, true
}

// buildLlamaBlockedResponse builds a complete, valid Llama-shaped InvokeModel
// response body for an INPUT guardrail block: the backend is never called, so
// token counts are zero and stop_reason reports the intervention.
func buildLlamaBlockedResponse(message string) ([]byte, error) {
	return json.Marshal(llamaInvokeResponse{
		Generation: message,
		StopReason: bedrockruntime.StopReasonGuardrailIntervened,
	})
}

// replaceLlamaCompletion rewrites body's generation wholesale with message
// and marks stop_reason as guardrail-intervened, for an OUTPUT block.
func replaceLlamaCompletion(body []byte, message string) ([]byte, error) {
	var lr llamaInvokeResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, err
	}
	lr.Generation = message
	lr.StopReason = bedrockruntime.StopReasonGuardrailIntervened
	return json.Marshal(lr)
}

// setLlamaCompletionText rewrites body's generation with text, preserving
// every other field (token counts, the backend's own stop_reason) verbatim,
// for an OUTPUT ANONYMIZE redaction.
func setLlamaCompletionText(body []byte, text string) ([]byte, error) {
	var lr llamaInvokeResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, err
	}
	lr.Generation = text
	return json.Marshal(lr)
}

// extractAnthropicPromptTexts parses a Bedrock-native Anthropic InvokeModel
// request body -- the wire shape anthropicRequest already models -- into its
// INPUT-checkable text: the system prompt plus every user message's text
// blocks, mirroring converseGuardrailTexts.
func extractAnthropicPromptTexts(body []byte) ([]string, bool) {
	var req anthropicRequest
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}
	var texts []string
	if req.System != "" {
		texts = append(texts, req.System)
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
	}
	return texts, true
}

// extractAnthropicCompletionTexts parses a Bedrock-native Anthropic
// InvokeModel response body into its OUTPUT-checkable text: every text
// content block, in order.
func extractAnthropicCompletionTexts(body []byte) ([]string, bool) {
	var resp anthropicResponse
	if json.Unmarshal(body, &resp) != nil {
		return nil, false
	}
	var texts []string
	for _, c := range resp.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	return texts, true
}

// buildAnthropicBlockedResponse builds a complete, valid Anthropic-shaped
// InvokeModel response body for an INPUT guardrail block: the backend is
// never called, so usage is zero and stop_reason reports the intervention.
func buildAnthropicBlockedResponse(modelID, message string) ([]byte, error) {
	return json.Marshal(struct {
		Type       string                  `json:"type"`
		Role       string                  `json:"role"`
		Model      string                  `json:"model"`
		Content    []anthropicContentBlock `json:"content"`
		StopReason string                  `json:"stop_reason"`
		Usage      anthropicUsage          `json:"usage"`
	}{
		Type:       "message",
		Role:       "assistant",
		Model:      anthropicModelID(modelID),
		Content:    []anthropicContentBlock{{Type: "text", Text: message}},
		StopReason: bedrockruntime.StopReasonGuardrailIntervened,
	})
}

// replaceAnthropicCompletion rewrites body's content wholesale with a single
// text block carrying message, and marks stop_reason as guardrail-intervened,
// preserving every other field (id, model, usage) verbatim -- InvokeModel
// forwards Anthropic's response bytes as-is, so a guardrail block must not
// lose fields it doesn't understand.
func replaceAnthropicCompletion(body []byte, message string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	contentJSON, err := json.Marshal([]anthropicContentBlock{{Type: "text", Text: message}})
	if err != nil {
		return nil, err
	}
	fields["content"] = contentJSON
	stopReasonJSON, err := json.Marshal(bedrockruntime.StopReasonGuardrailIntervened)
	if err != nil {
		return nil, err
	}
	fields["stop_reason"] = stopReasonJSON
	return json.Marshal(fields)
}

// setAnthropicCompletionTexts rewrites body's text content blocks in place
// with texts, in order, preserving every other field (id, model, stop_reason,
// usage) verbatim, for an OUTPUT ANONYMIZE redaction.
func setAnthropicCompletionTexts(body []byte, texts []string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	var blocks []anthropicContentBlock
	if raw, ok := fields["content"]; ok {
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, err
		}
	}
	i := 0
	for bi := range blocks {
		if blocks[bi].Type != "text" {
			continue
		}
		if i < len(texts) {
			blocks[bi].Text = texts[i]
		}
		i++
	}
	contentJSON, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	fields["content"] = contentJSON
	return json.Marshal(fields)
}

// extractInvokePromptTexts dispatches to the family-specific INPUT-text
// extractor for backend (an InvocationRecord/catalog Provider tag), mirroring
// extractTokenUsage's own backend switch. ok is false when body doesn't
// decode as backend's shape or backend has no guardrail support.
func extractInvokePromptTexts(backend string, body []byte) (texts []string, ok bool) {
	switch {
	case backend == tierSelfHost:
		return extractLlamaPromptTexts(body)
	case strings.HasPrefix(backend, providerPrefix):
		switch strings.TrimPrefix(backend, providerPrefix) {
		case vendorAnthropic:
			return extractAnthropicPromptTexts(body)
		}
	}
	return nil, false
}

// extractInvokeCompletionTexts is extractInvokePromptTexts' OUTPUT sibling.
func extractInvokeCompletionTexts(backend string, body []byte) (texts []string, ok bool) {
	switch {
	case backend == tierSelfHost:
		return extractLlamaCompletionTexts(body)
	case strings.HasPrefix(backend, providerPrefix):
		switch strings.TrimPrefix(backend, providerPrefix) {
		case vendorAnthropic:
			return extractAnthropicCompletionTexts(body)
		}
	}
	return nil, false
}

// invokeGuardrailBlockedResponse builds a complete, valid family-native
// InvokeModel response body carrying message as the assistant text, for an
// INPUT guardrail block -- the backend is never called.
func invokeGuardrailBlockedResponse(backend, modelID, message string) ([]byte, error) {
	switch {
	case backend == tierSelfHost:
		return buildLlamaBlockedResponse(message)
	case strings.HasPrefix(backend, providerPrefix):
		switch strings.TrimPrefix(backend, providerPrefix) {
		case vendorAnthropic:
			return buildAnthropicBlockedResponse(modelID, message)
		}
	}
	return nil, errors.New(awserrors.ErrorInternalError)
}

// invokeGuardrailBlockedCompletion rewrites respBody's assistant text
// wholesale with message, for an OUTPUT guardrail block.
func invokeGuardrailBlockedCompletion(backend string, respBody []byte, message string) ([]byte, error) {
	switch {
	case backend == tierSelfHost:
		return replaceLlamaCompletion(respBody, message)
	case strings.HasPrefix(backend, providerPrefix):
		switch strings.TrimPrefix(backend, providerPrefix) {
		case vendorAnthropic:
			return replaceAnthropicCompletion(respBody, message)
		}
	}
	return respBody, nil
}

// invokeGuardrailRedactedCompletion rewrites respBody's assistant text
// positionally with texts, for an OUTPUT ANONYMIZE redaction.
func invokeGuardrailRedactedCompletion(backend string, respBody []byte, texts []string) ([]byte, error) {
	switch {
	case backend == tierSelfHost:
		if len(texts) == 0 {
			return respBody, nil
		}
		return setLlamaCompletionText(respBody, texts[0])
	case strings.HasPrefix(backend, providerPrefix):
		switch strings.TrimPrefix(backend, providerPrefix) {
		case vendorAnthropic:
			return setAnthropicCompletionTexts(respBody, texts)
		}
	}
	return respBody, nil
}

// extractInvokeStreamChunkText pulls the assistant-text delta from one raw
// invoke-stream chunk for buffered OUTPUT accumulation. ok=false is a benign
// no-text chunk; decodeErr=true is a real JSON-decode failure (fail closed).
func extractInvokeStreamChunkText(backend string, chunk []byte) (text string, ok bool, decodeErr bool) {
	switch {
	case backend == tierSelfHost:
		var c struct {
			Generation string `json:"generation"`
		}
		if json.Unmarshal(chunk, &c) != nil {
			return "", false, true
		}
		return c.Generation, c.Generation != "", false
	case strings.HasPrefix(backend, providerPrefix):
		switch strings.TrimPrefix(backend, providerPrefix) {
		case vendorAnthropic:
			var c struct {
				Type  string `json:"type"`
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if json.Unmarshal(chunk, &c) != nil {
				return "", false, true
			}
			if c.Type != "content_block_delta" {
				return "", false, false
			}
			return c.Delta.Text, c.Delta.Text != "", false
		}
	}
	return "", false, false
}

// llamaGuardedStreamChunks builds the two-chunk (delta + final) Llama
// invoke-stream sequence carrying message as the whole guarded generation.
func llamaGuardedStreamChunks(message string, blocked bool) ([][]byte, error) {
	delta, err := json.Marshal(llamaInvokeStreamChunk{Generation: message})
	if err != nil {
		return nil, err
	}
	stopReason := "stop"
	if blocked {
		stopReason = bedrockruntime.StopReasonGuardrailIntervened
	}
	final, err := json.Marshal(llamaInvokeStreamFinalChunk{StopReason: stopReason})
	if err != nil {
		return nil, err
	}
	return [][]byte{delta, final}, nil
}

// anthropicGuardedStreamChunks builds a minimal valid Anthropic invoke-stream
// event sequence (content_block_start/delta/stop, message_delta,
// message_stop) carrying message as the whole guarded assistant text.
func anthropicGuardedStreamChunks(message string, blocked bool) ([][]byte, error) {
	contentStart, err := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	if err != nil {
		return nil, err
	}
	delta, err := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]string{"type": "text_delta", "text": message},
	})
	if err != nil {
		return nil, err
	}
	contentStop, err := json.Marshal(map[string]any{"type": "content_block_stop", "index": 0})
	if err != nil {
		return nil, err
	}
	stopReason := bedrockruntime.StopReasonEndTurn
	if blocked {
		stopReason = bedrockruntime.StopReasonGuardrailIntervened
	}
	msgDelta, err := json.Marshal(map[string]any{
		"type": "message_delta", "delta": map[string]string{"stop_reason": stopReason},
	})
	if err != nil {
		return nil, err
	}
	msgStop, err := json.Marshal(map[string]string{"type": "message_stop"})
	if err != nil {
		return nil, err
	}
	return [][]byte{contentStart, delta, contentStop, msgDelta, msgStop}, nil
}

// buildGuardedInvokeStreamChunks dispatches to the family-specific guarded
// invoke-stream chunk builder for backend.
func buildGuardedInvokeStreamChunks(backend, message string, blocked bool) ([][]byte, error) {
	switch {
	case backend == tierSelfHost:
		return llamaGuardedStreamChunks(message, blocked)
	case strings.HasPrefix(backend, providerPrefix):
		switch strings.TrimPrefix(backend, providerPrefix) {
		case vendorAnthropic:
			return anthropicGuardedStreamChunks(message, blocked)
		}
	}
	return nil, errors.New(awserrors.ErrorInternalError)
}

// blockedInvokeStreamSource yields exactly the guarded chunks built for an
// INPUT guardrail block, then EOF; the backend stream is never opened.
type blockedInvokeStreamSource struct {
	chunks [][]byte
}

var _ invokeStreamSource = (*blockedInvokeStreamSource)(nil)

func (b *blockedInvokeStreamSource) Next(_ context.Context) ([]byte, bool, error) {
	if len(b.chunks) == 0 {
		return nil, false, nil
	}
	c := b.chunks[0]
	b.chunks = b.chunks[1:]
	return c, true, nil
}

func (b *blockedInvokeStreamSource) Close() error { return nil }

// guardrailInvokeStreamSource wraps an invokeStreamSource for buffered/SYNC
// OUTPUT enforcement: every raw chunk is withheld and its assistant-text
// accumulated until the inner stream ends, then assessed once and replaced
// by a guarded chunk sequence before forwarding -- the invoke-stream
// counterpart of guardrailStreamSource.
type guardrailInvokeStreamSource struct {
	inner   invokeStreamSource
	backend string

	store     *GuardrailStore
	embedder  Embedder
	accountID string
	ident     string
	version   string

	text     strings.Builder
	buffered [][]byte
	queue    [][]byte
	assessed bool
}

// newGuardrailInvokeStreamSource constructs a guardrailInvokeStreamSource.
// embedder drives topicPolicy's semantic match on the OUTPUT check below.
func newGuardrailInvokeStreamSource(inner invokeStreamSource, store *GuardrailStore, embedder Embedder, accountID, ident, version, backend string) *guardrailInvokeStreamSource {
	return &guardrailInvokeStreamSource{inner: inner, store: store, embedder: embedder, accountID: accountID, ident: ident, version: version, backend: backend}
}

var _ invokeStreamSource = (*guardrailInvokeStreamSource)(nil)
var _ usageReporter = (*guardrailInvokeStreamSource)(nil)

func (g *guardrailInvokeStreamSource) Close() error {
	return g.inner.Close()
}

// usage delegates to the wrapped source when it reports real usage,
// mirroring guardrailStreamSource.usage.
func (g *guardrailInvokeStreamSource) usage() (inputTokens, outputTokens int64, estimated bool) {
	if ur, ok := g.inner.(usageReporter); ok {
		return ur.usage()
	}
	return 0, 0, true
}

// Next drains and buffers the entire inner stream on first call, assesses
// the accumulated text once the inner stream ends, then serves the guarded
// (or, when nothing triggered, the original unmodified) chunk sequence.
func (g *guardrailInvokeStreamSource) Next(ctx context.Context) ([]byte, bool, error) {
	if len(g.queue) > 0 {
		c := g.queue[0]
		g.queue = g.queue[1:]
		return c, true, nil
	}
	if g.assessed {
		return nil, false, nil
	}

	var extractionFailed bool
	for {
		chunk, ok, err := g.inner.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			break
		}
		text, hasText, decodeErr := extractInvokeStreamChunkText(g.backend, chunk)
		if decodeErr {
			extractionFailed = true
			break
		}
		g.buffered = append(g.buffered, chunk)
		if hasText {
			g.text.WriteString(text)
		}
	}
	g.assessed = true

	if extractionFailed {
		slog.Error("invoke-stream: failed to decode assistant chunk for guardrail OUTPUT check, blocking", "backend", g.backend)
		view, verr := loadGuardrailView(ctx, g.store, g.accountID, g.ident, g.version)
		if verr != nil {
			return nil, false, verr
		}
		var berr error
		g.queue, berr = buildGuardedInvokeStreamChunks(g.backend, view.BlockedOutputsMessaging, true)
		if berr != nil {
			return nil, false, berr
		}
		return g.Next(ctx)
	}

	original := g.text.String()
	blocked, message, redacted, _, err := enforceGuardrail(ctx, g.store, g.embedder, g.accountID, g.ident, g.version,
		bedrockruntime.GuardrailContentSourceOutput, []string{original})
	if err != nil {
		return nil, false, err
	}

	switch {
	case blocked:
		g.queue, err = buildGuardedInvokeStreamChunks(g.backend, message, true)
	case len(redacted) > 0 && redacted[0] != original:
		g.queue, err = buildGuardedInvokeStreamChunks(g.backend, redacted[0], false)
	default:
		g.queue = g.buffered
	}
	if err != nil {
		return nil, false, err
	}
	return g.Next(ctx)
}
