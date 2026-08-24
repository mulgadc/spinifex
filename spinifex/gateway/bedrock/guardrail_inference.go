package gateway_bedrock

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
)

// loadGuardrailView resolves ident to the stored record's configured view for
// version (DRAFT when empty), the resolve->load->version-select prologue shared
// with enforceGuardrail. An unresolvable or foreign guardrail fails closed.
func loadGuardrailView(ctx context.Context, store *GuardrailStore, accountID, ident, version string) (guardrailView, error) {
	if store == nil || ident == "" {
		return guardrailView{}, errGuardrailNotFound(ident, "")
	}
	id, err := resolveGuardrailID(ident, store.region, accountID)
	if err != nil {
		return guardrailView{}, err
	}

	kv, err := store.bucket(ctx)
	if err != nil {
		return guardrailView{}, err
	}
	rec, found, err := getGuardrailRecord(ctx, kv, accountID, id)
	if err != nil {
		return guardrailView{}, err
	}
	if !found {
		return guardrailView{}, errGuardrailNotFound(id, "")
	}

	view := rec.guardrailView
	if version != "" && version != guardrailDraftVersion {
		snap, ok := rec.Versions[version]
		if !ok {
			return guardrailView{}, errGuardrailNotFound(id, version)
		}
		view = snap.guardrailView
	}
	return view, nil
}

// enforceGuardrail is the single resolve->load->filter path Converse and
// ConverseStream both call, mirroring ApplyGuardrail's own sequence. An
// unresolvable or foreign guardrail fails closed: ResourceNotFoundException.
func enforceGuardrail(ctx context.Context, store *GuardrailStore, accountID, ident, version, source string, texts []string) (blocked bool, blockedMessage string, redactedTexts []string, assessments []*bedrockruntime.GuardrailAssessment, err error) {
	view, err := loadGuardrailView(ctx, store, accountID, ident, version)
	if err != nil {
		return false, "", texts, nil, err
	}

	action, gassessments, outputs, _ := applyGuardrailPolicies(view, texts, source)
	if action == bedrockruntime.GuardrailActionGuardrailIntervened {
		message := view.BlockedInputMessaging
		if source == bedrockruntime.GuardrailContentSourceOutput {
			message = view.BlockedOutputsMessaging
		}
		return true, message, texts, gassessments, nil
	}
	return false, "", outputs, gassessments, nil
}

// converseGuardrailTexts collects the INPUT-checkable text from a Converse
// request: every system prompt block plus every user-role message's text,
// matching AWS's default of assessing the whole message.
func converseGuardrailTexts(input *bedrockruntime.ConverseInput) []string {
	var texts []string
	for _, s := range input.System {
		if s != nil && s.Text != nil {
			texts = append(texts, aws.StringValue(s.Text))
		}
	}
	for _, m := range input.Messages {
		if m == nil || aws.StringValue(m.Role) != bedrockruntime.ConversationRoleUser {
			continue
		}
		for _, c := range m.Content {
			if c != nil && c.Text != nil {
				texts = append(texts, aws.StringValue(c.Text))
			}
		}
	}
	return texts
}

// streamGuardrailTexts is converseGuardrailTexts' sibling for
// ConverseStreamInput: the two request shapes are field-identical for
// Messages/System, only the generated Go type name differs.
func streamGuardrailTexts(input *bedrockruntime.ConverseStreamInput) []string {
	var texts []string
	for _, s := range input.System {
		if s != nil && s.Text != nil {
			texts = append(texts, aws.StringValue(s.Text))
		}
	}
	for _, m := range input.Messages {
		if m == nil || aws.StringValue(m.Role) != bedrockruntime.ConversationRoleUser {
			continue
		}
		for _, c := range m.Content {
			if c != nil && c.Text != nil {
				texts = append(texts, aws.StringValue(c.Text))
			}
		}
	}
	return texts
}

// converseOutputTexts collects the OUTPUT-checkable text from a completed
// ConverseOutput's assistant message.
func converseOutputTexts(out *bedrockruntime.ConverseOutput) []string {
	if out == nil || out.Output == nil || out.Output.Message == nil {
		return nil
	}
	var texts []string
	for _, c := range out.Output.Message.Content {
		if c != nil && c.Text != nil {
			texts = append(texts, aws.StringValue(c.Text))
		}
	}
	return texts
}

// setConverseOutputTexts writes texts back into out's assistant message text
// blocks, in order, substituting the (possibly ANONYMIZE-redacted) text
// applyGuardrailPolicies returned for each original text block.
func setConverseOutputTexts(out *bedrockruntime.ConverseOutput, texts []string) {
	if out == nil || out.Output == nil || out.Output.Message == nil {
		return
	}
	i := 0
	for _, c := range out.Output.Message.Content {
		if c == nil || c.Text == nil {
			continue
		}
		if i < len(texts) {
			c.Text = aws.String(texts[i])
		}
		i++
	}
}

// blockedConverseOutput builds a complete, valid ConverseOutput for an INPUT
// guardrail block: the backend is never called, so usage is zero and latency
// reflects only the time spent resolving and evaluating the guardrail.
func blockedConverseOutput(message string, latency time.Duration) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output: &bedrockruntime.ConverseOutput_{
			Message: &bedrockruntime.Message{
				Role:    aws.String(bedrockruntime.ConversationRoleAssistant),
				Content: []*bedrockruntime.ContentBlock{{Text: aws.String(message)}},
			},
		},
		StopReason: aws.String(bedrockruntime.StopReasonGuardrailIntervened),
		Usage: &bedrockruntime.TokenUsage{
			InputTokens:  aws.Int64(0),
			OutputTokens: aws.Int64(0),
			TotalTokens:  aws.Int64(0),
		},
		Metrics: &bedrockruntime.ConverseMetrics{LatencyMs: aws.Int64(latency.Milliseconds())},
	}
}

// converseGuardrailTrace builds the ConverseTrace AWS returns when
// GuardrailConfig.Trace is "enabled", keyed by the resolved guardrail id, or
// nil when neither check produced an assessment to report.
func converseGuardrailTrace(guardrailID string, inputAssessments, outputAssessments []*bedrockruntime.GuardrailAssessment) *bedrockruntime.ConverseTrace {
	if len(inputAssessments) == 0 && len(outputAssessments) == 0 {
		return nil
	}
	assessment := &bedrockruntime.GuardrailTraceAssessment{}
	if len(inputAssessments) > 0 {
		assessment.InputAssessment = map[string]*bedrockruntime.GuardrailAssessment{guardrailID: inputAssessments[0]}
	}
	if len(outputAssessments) > 0 {
		assessment.OutputAssessments = map[string][]*bedrockruntime.GuardrailAssessment{guardrailID: outputAssessments}
	}
	return &bedrockruntime.ConverseTrace{Guardrail: assessment}
}

// writeBlockedConverseStream emits the minimal guarded event sequence for a
// pre-stream INPUT block (messageStart..messageStop plus metadata); the
// backend is never opened. Records one InvocationRecord, own exit path.
func writeBlockedConverseStream(ctx context.Context, w http.ResponseWriter, modelID string, rc streamRecordCtx, message string, traceEnabled bool, guardrailID string, assessments []*bedrockruntime.GuardrailAssessment) error {
	fw, err := newFrameWriter(w)
	if err != nil {
		return err
	}

	metadata := &bedrockruntime.ConverseStreamMetadataEvent{
		Usage:   &bedrockruntime.TokenUsage{InputTokens: aws.Int64(0), OutputTokens: aws.Int64(0), TotalTokens: aws.Int64(0)},
		Metrics: &bedrockruntime.ConverseStreamMetrics{LatencyMs: aws.Int64(time.Since(rc.start).Milliseconds())},
	}
	if traceEnabled && len(assessments) > 0 {
		metadata.Trace = &bedrockruntime.ConverseStreamTrace{
			Guardrail: &bedrockruntime.GuardrailTraceAssessment{
				InputAssessment: map[string]*bedrockruntime.GuardrailAssessment{guardrailID: assessments[0]},
			},
		}
	}

	events := []ConverseStreamEvent{
		{Kind: converseStreamEventMessageStart, MessageStart: &bedrockruntime.MessageStartEvent{Role: aws.String(bedrockruntime.ConversationRoleAssistant)}},
		{Kind: converseStreamEventContentBlockDelta, ContentBlockDelta: &bedrockruntime.ContentBlockDeltaEvent{ContentBlockIndex: aws.Int64(0), Delta: &bedrockruntime.ContentBlockDelta{Text: aws.String(message)}}},
		{Kind: converseStreamEventContentBlockStop, ContentBlockStop: &bedrockruntime.ContentBlockStopEvent{ContentBlockIndex: aws.Int64(0)}},
		{Kind: converseStreamEventMessageStop, MessageStop: &bedrockruntime.MessageStopEvent{StopReason: aws.String(bedrockruntime.StopReasonGuardrailIntervened)}},
		{Kind: converseStreamEventMetadata, Metadata: metadata},
	}

	for _, event := range events {
		payload, perr := event.payload()
		if perr != nil {
			slog.Error("converse-stream: failed to marshal guarded event", "model", modelID, "kind", event.Kind, "err", perr)
			break
		}
		if werr := fw.writeEvent(event.Kind.eventType(), payload); werr != nil {
			slog.Error("converse-stream: failed to write guarded frame, aborting", "model", modelID, "err", werr)
			break
		}
	}

	rc.recorder.Record(ctx, InvocationRecord{
		RequestID:  rc.requestID,
		AccountID:  rc.accountID,
		ModelID:    modelID,
		Operation:  rc.operation,
		Backend:    rc.backend,
		LatencyMs:  time.Since(rc.start).Milliseconds(),
		HTTPStatus: http.StatusOK,
		InputText:  rc.inputText,
		OutputText: message,
	})
	return nil
}

// guardrailStreamSource wraps a converseStreamSource for buffered/SYNC OUTPUT
// enforcement: every contentBlockDelta is held until its block closes, then
// assessed once and replaced by a single guarded delta before forwarding.
type guardrailStreamSource struct {
	inner converseStreamSource

	store     *GuardrailStore
	accountID string
	ident     string
	version   string
	trace     bool

	inputAssessments []*bedrockruntime.GuardrailAssessment

	text        strings.Builder
	blockIndex  *int64
	queue       []ConverseStreamEvent
	assessed    bool
	blocked     bool
	assessErr   error
	assessments []*bedrockruntime.GuardrailAssessment
}

// newGuardrailStreamSource constructs a guardrailStreamSource. inputAssessments
// is the already-computed result of the pre-stream INPUT check, carried
// through so the trailing metadata event's trace can report both halves.
func newGuardrailStreamSource(inner converseStreamSource, store *GuardrailStore, accountID, ident, version string, trace bool, inputAssessments []*bedrockruntime.GuardrailAssessment) *guardrailStreamSource {
	return &guardrailStreamSource{inner: inner, store: store, accountID: accountID, ident: ident, version: version, trace: trace, inputAssessments: inputAssessments}
}

var _ converseStreamSource = (*guardrailStreamSource)(nil)
var _ usageReporter = (*guardrailStreamSource)(nil)

func (g *guardrailStreamSource) Close() error {
	return g.inner.Close()
}

// usage delegates to the wrapped source when it reports real usage,
// otherwise falls back to the same zero/estimated contract pumpConverseStream
// already applies to every non-usageReporter source.
func (g *guardrailStreamSource) usage() (inputTokens, outputTokens int64, estimated bool) {
	if ur, ok := g.inner.(usageReporter); ok {
		return ur.usage()
	}
	return 0, 0, true
}

// assess runs the OUTPUT guardrail check exactly once, over the full
// accumulated delta text, and rewrites g.text in place to the guarded
// (blocked-message or possibly ANONYMIZE-redacted) replacement.
func (g *guardrailStreamSource) assess(ctx context.Context) {
	if g.assessed {
		return
	}
	g.assessed = true
	blocked, message, redacted, assessments, err := enforceGuardrail(ctx, g.store, g.accountID, g.ident, g.version,
		bedrockruntime.GuardrailContentSourceOutput, []string{g.text.String()})
	if err != nil {
		g.assessErr = err
		return
	}
	g.blocked = blocked
	g.assessments = assessments
	g.text.Reset()
	switch {
	case blocked:
		g.text.WriteString(message)
	case len(redacted) > 0:
		g.text.WriteString(redacted[0])
	}
}

// guardedDeltaEvent is the single synthetic contentBlockDelta emitted in
// place of every raw delta a block suppressed, carrying the post-assessment
// text.
func (g *guardrailStreamSource) guardedDeltaEvent() ConverseStreamEvent {
	return ConverseStreamEvent{
		Kind: converseStreamEventContentBlockDelta,
		ContentBlockDelta: &bedrockruntime.ContentBlockDeltaEvent{
			ContentBlockIndex: g.blockIndexOrZero(),
			Delta:             &bedrockruntime.ContentBlockDelta{Text: aws.String(g.text.String())},
		},
	}
}

func (g *guardrailStreamSource) blockIndexOrZero() *int64 {
	if g.blockIndex != nil {
		return g.blockIndex
	}
	return aws.Int64(0)
}

// Next drains inner, buffering contentBlockDelta text until the block closes
// (or, defensively, messageStop), assesses it once, queues the guarded delta
// ahead of the stop event, and overrides StopReason when it blocked.
func (g *guardrailStreamSource) Next(ctx context.Context) (ConverseStreamEvent, bool, error) {
	if len(g.queue) > 0 {
		event := g.queue[0]
		g.queue = g.queue[1:]
		return event, true, nil
	}

	for {
		event, ok, err := g.inner.Next(ctx)
		if err != nil || !ok {
			return event, ok, err
		}

		switch event.Kind {
		case converseStreamEventContentBlockDelta:
			if event.ContentBlockDelta != nil {
				if g.blockIndex == nil {
					g.blockIndex = event.ContentBlockDelta.ContentBlockIndex
				}
				if event.ContentBlockDelta.Delta != nil {
					g.text.WriteString(aws.StringValue(event.ContentBlockDelta.Delta.Text))
				}
			}
			continue

		case converseStreamEventContentBlockStop:
			g.assess(ctx)
			if g.assessErr != nil {
				return ConverseStreamEvent{}, false, g.assessErr
			}
			return g.dequeue(g.guardedDeltaEvent(), event), true, nil

		case converseStreamEventMessageStop:
			if !g.assessed {
				g.assess(ctx)
				if g.assessErr != nil {
					return ConverseStreamEvent{}, false, g.assessErr
				}
				g.queue = append(g.queue, g.guardedDeltaEvent(),
					ConverseStreamEvent{Kind: converseStreamEventContentBlockStop, ContentBlockStop: &bedrockruntime.ContentBlockStopEvent{ContentBlockIndex: g.blockIndexOrZero()}})
			}
			if g.blocked {
				event.MessageStop = &bedrockruntime.MessageStopEvent{StopReason: aws.String(bedrockruntime.StopReasonGuardrailIntervened)}
			}
			return g.dequeue(event), true, nil

		case converseStreamEventMetadata:
			if g.trace && event.Metadata != nil {
				event.Metadata.Trace = g.buildTrace()
			}
			return event, true, nil

		default:
			return event, true, nil
		}
	}
}

// dequeue appends events to g.queue and pops+returns the first one, the
// shared tail of Next's contentBlockStop and messageStop branches.
func (g *guardrailStreamSource) dequeue(events ...ConverseStreamEvent) ConverseStreamEvent {
	g.queue = append(g.queue, events...)
	next := g.queue[0]
	g.queue = g.queue[1:]
	return next
}

// buildTrace merges the pre-stream INPUT assessment with the OUTPUT
// assessment computed at block-close, mirroring Converse's own
// converseGuardrailTrace. Returns nil when neither ran.
func (g *guardrailStreamSource) buildTrace() *bedrockruntime.ConverseStreamTrace {
	if len(g.inputAssessments) == 0 && len(g.assessments) == 0 {
		return nil
	}
	assessment := &bedrockruntime.GuardrailTraceAssessment{}
	if len(g.inputAssessments) > 0 {
		assessment.InputAssessment = map[string]*bedrockruntime.GuardrailAssessment{g.ident: g.inputAssessments[0]}
	}
	if len(g.assessments) > 0 {
		assessment.OutputAssessments = map[string][]*bedrockruntime.GuardrailAssessment{g.ident: g.assessments}
	}
	return &bedrockruntime.ConverseStreamTrace{Guardrail: assessment}
}
