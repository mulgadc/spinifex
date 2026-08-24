package gateway_bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// converseStreamEventKind tags which aws-sdk-go v1 event struct a
// ConverseStreamEvent carries, driving both the frame's ":event-type" header
// and which field payload() marshals.
type converseStreamEventKind int

const (
	converseStreamEventMessageStart converseStreamEventKind = iota
	converseStreamEventContentBlockStart
	converseStreamEventContentBlockDelta
	converseStreamEventContentBlockStop
	converseStreamEventMessageStop
	converseStreamEventMetadata
)

// eventType is the Bedrock wire ":event-type" for kind.
func (k converseStreamEventKind) eventType() string {
	switch k {
	case converseStreamEventMessageStart:
		return "messageStart"
	case converseStreamEventContentBlockStart:
		return "contentBlockStart"
	case converseStreamEventContentBlockDelta:
		return "contentBlockDelta"
	case converseStreamEventContentBlockStop:
		return "contentBlockStop"
	case converseStreamEventMessageStop:
		return "messageStop"
	case converseStreamEventMetadata:
		return "metadata"
	default:
		return "unknown"
	}
}

// ConverseStreamEvent is one normalized Bedrock ConverseStream event. Exactly
// one payload field is set, matching Kind; reframers (vLLM, Anthropic) build
// these from their own SSE so the writer/pump stays backend-agnostic.
type ConverseStreamEvent struct {
	Kind              converseStreamEventKind
	MessageStart      *bedrockruntime.MessageStartEvent
	ContentBlockStart *bedrockruntime.ContentBlockStartEvent
	ContentBlockDelta *bedrockruntime.ContentBlockDeltaEvent
	ContentBlockStop  *bedrockruntime.ContentBlockStopEvent
	MessageStop       *bedrockruntime.MessageStopEvent
	Metadata          *bedrockruntime.ConverseStreamMetadataEvent
}

// payload JSON-marshals the event struct matching Kind via jsonutil.BuildJSON
// (the SDK's own restjson marshaler), honouring the locationName tags a real
// SDK consumer's UnmarshalEvent expects — mirroring WriteJSONResponse.
func (e ConverseStreamEvent) payload() ([]byte, error) {
	switch e.Kind {
	case converseStreamEventMessageStart:
		return jsonutil.BuildJSON(e.MessageStart)
	case converseStreamEventContentBlockStart:
		return jsonutil.BuildJSON(e.ContentBlockStart)
	case converseStreamEventContentBlockDelta:
		return jsonutil.BuildJSON(e.ContentBlockDelta)
	case converseStreamEventContentBlockStop:
		return jsonutil.BuildJSON(e.ContentBlockStop)
	case converseStreamEventMessageStop:
		return jsonutil.BuildJSON(e.MessageStop)
	case converseStreamEventMetadata:
		return jsonutil.BuildJSON(e.Metadata)
	default:
		return nil, fmt.Errorf("converse stream: unknown event kind %d", e.Kind)
	}
}

// converseStreamSource is the backend-agnostic reframer contract: Next
// returns the next normalized event, or (zero,false,nil) at clean EOF.
type converseStreamSource interface {
	Next(ctx context.Context) (ConverseStreamEvent, bool, error)
	Close() error
}

// ConverseStream is the bedrock-runtime ConverseStream entry point used by
// the gateway route table. Unlike the JSON-dispatch handlers it owns w
// directly: a pre-stream failure (unknown/ungranted model, unresolved
// credential, upstream connect error, unresolvable GuardrailConfig) returns
// an awserrors code for the normal ErrorHandler envelope; once the first
// frame is written it always returns nil, surfacing later failures as an
// in-band exception event instead. provisioned and guardrails may be nil,
// which fails closed on a PT ARN or GuardrailConfig respectively.
func ConverseStream(ctx context.Context, w http.ResponseWriter, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore, guardrails *GuardrailStore) error {
	if recorder == nil {
		recorder = NoopRecorder
	}
	requestID := uuid.NewV4().String()
	start := time.Now()

	input := new(bedrockruntime.ConverseStreamInput)
	if len(body) > 0 {
		if err := json.Unmarshal(body, input); err != nil {
			return errors.New(awserrors.ErrorValidationException)
		}
	}

	// Resolved here too (Router.ConverseStream below resolves it again for
	// routing) purely so entry's Provider tag reflects a PT ARN's target
	// model, not the raw ARN, for the InvocationRecord's Backend field.
	_, recordModelID, _ := resolveInferenceTarget(ctx, accountID, modelID, provisioned)
	entry, _ := lookupCatalogEntry(recordModelID) // Router.ConverseStream below re-validates; only its Provider tag is needed here.

	inputJSON, jerr := json.Marshal(input)
	if jerr != nil {
		slog.Error("converse-stream: failed to marshal input for recording", "model", modelID, "err", jerr)
	}
	rc := streamRecordCtx{
		recorder:  recorder,
		requestID: requestID,
		accountID: accountID,
		backend:   entry.Provider,
		operation: OperationConverseStream,
		start:     start,
		inputText: string(inputJSON),
	}

	// Guardrail INPUT check happens before the router ever resolves a
	// backend: a request naming a guardrail that blocks the input must never
	// open a connection to the model, self-host or Anthropic alike.
	gc := input.GuardrailConfig
	var inputAssessments []*bedrockruntime.GuardrailAssessment
	traceEnabled := gc != nil && aws.StringValue(gc.Trace) == bedrockruntime.GuardrailTraceEnabled
	if gc != nil {
		ident, version := aws.StringValue(gc.GuardrailIdentifier), aws.StringValue(gc.GuardrailVersion)
		blocked, message, _, assessments, err := enforceGuardrail(ctx, guardrails, accountID, ident, version,
			bedrockruntime.GuardrailContentSourceInput, streamGuardrailTexts(input))
		if err != nil {
			return err
		}
		inputAssessments = assessments
		if blocked {
			return writeBlockedConverseStream(ctx, w, modelID, rc, message, traceEnabled, ident, assessments)
		}
	}

	src, err := NewRouter(resolver, endpointResolver, recorder, access, provisioned, guardrails).ConverseStream(ctx, accountID, modelID, input)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); cerr != nil {
			slog.Error("converse-stream: failed to close upstream source", "model", modelID, "err", cerr)
		}
	}()

	// OUTPUT enforcement is buffered/SYNC: guardrailStreamSource holds every
	// delta until its block closes rather than letting pumpConverseStream
	// forward unassessed model text.
	if gc != nil {
		ident, version := aws.StringValue(gc.GuardrailIdentifier), aws.StringValue(gc.GuardrailVersion)
		src = newGuardrailStreamSource(src, guardrails, accountID, ident, version, traceEnabled, inputAssessments)
	}

	fw, err := newFrameWriter(w)
	if err != nil {
		return err
	}

	pumpConverseStream(ctx, fw, src, modelID, rc)
	return nil
}

// pumpConverseStream drains src and writes each normalized event as a frame,
// in AWS order (messageStart -> per block contentBlockStart/Delta*/Stop ->
// messageStop -> metadata). A mid-stream Next error surfaces as an in-band
// exception event and stops the pump; a write/flush error also stops the
// pump silently, since the client connection is already broken and the
// HTTP status can no longer change either way. On every exit — clean end,
// client disconnect (ctx.Done), or upstream fault — the deferred closure
// records exactly one InvocationRecord via rc.recorder.
func pumpConverseStream(ctx context.Context, fw *frameWriter, src converseStreamSource, modelID string, rc streamRecordCtx) {
	if rc.recorder == nil {
		rc.recorder = NoopRecorder
	}
	partial := true
	errCode := ""
	var outputText strings.Builder

	defer func() {
		inputTokens, outputTokens, estimated := int64(0), int64(0), true
		if ur, ok := src.(usageReporter); ok {
			inputTokens, outputTokens, estimated = ur.usage()
		}
		rc.recorder.Record(ctx, InvocationRecord{
			RequestID:      rc.requestID,
			AccountID:      rc.accountID,
			ModelID:        modelID,
			Operation:      rc.operation,
			Backend:        rc.backend,
			LatencyMs:      time.Since(rc.start).Milliseconds(),
			HTTPStatus:     http.StatusOK, // the 200 header is already committed by the time the pump runs
			ErrorCode:      errCode,
			InputTokens:    inputTokens,
			OutputTokens:   outputTokens,
			UsageEstimated: estimated,
			Partial:        partial,
			InputText:      rc.inputText,
			OutputText:     outputText.String(),
		})
	}()

	for {
		select {
		case <-ctx.Done():
			errCode = errCodeClientDisconnected
			return
		default:
		}

		event, ok, err := src.Next(ctx)
		if err != nil {
			excType := excInternalServerException
			if _, ok := errors.AsType[*streamFaultError](err); ok {
				excType = excModelStreamErrorException
			}
			errCode = awserrors.ValidErrorCodeFromError(err)
			slog.Error("converse-stream: upstream fault", "model", modelID, "err", err)
			if werr := fw.writeException(excType, exceptionPayload(err)); werr != nil {
				slog.Error("converse-stream: failed to write exception frame", "model", modelID, "err", werr)
				errCode = errCodeClientDisconnected
			}
			return
		}
		if !ok {
			partial = false
			return
		}

		if event.Kind == converseStreamEventContentBlockDelta && event.ContentBlockDelta != nil && event.ContentBlockDelta.Delta != nil {
			outputText.WriteString(aws.StringValue(event.ContentBlockDelta.Delta.Text))
		}

		payload, err := event.payload()
		if err != nil {
			errCode = awserrors.ErrorInternalError
			slog.Error("converse-stream: failed to marshal event", "model", modelID, "kind", event.Kind, "err", err)
			if werr := fw.writeException(excInternalServerException, exceptionPayload(err)); werr != nil {
				slog.Error("converse-stream: failed to write exception frame", "model", modelID, "err", werr)
			}
			return
		}

		if werr := fw.writeEvent(event.Kind.eventType(), payload); werr != nil {
			errCode = errCodeClientDisconnected
			slog.Error("converse-stream: failed to write frame, aborting", "model", modelID, "err", werr)
			return
		}
	}
}
