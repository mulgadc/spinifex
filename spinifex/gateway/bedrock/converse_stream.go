package gateway_bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

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
// directly: a pre-stream failure (unknown model, ungranted model, unresolved
// credential, upstream connect error) returns an awserrors code for the normal
// ErrorHandler envelope. Once the first frame is written it always returns
// nil — any later failure is an in-band exception event, since the HTTP
// status can no longer change.
func ConverseStream(ctx context.Context, w http.ResponseWriter, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, access AccessResolver) error {
	input := new(bedrockruntime.ConverseStreamInput)
	if len(body) > 0 {
		if err := json.Unmarshal(body, input); err != nil {
			return errors.New(awserrors.ErrorValidationException)
		}
	}

	src, err := NewRouter(resolver, endpointResolver, access).ConverseStream(ctx, accountID, modelID, input)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); cerr != nil {
			slog.Error("converse-stream: failed to close upstream source", "model", modelID, "err", cerr)
		}
	}()

	fw, err := newFrameWriter(w)
	if err != nil {
		return err
	}

	pumpConverseStream(ctx, fw, src, modelID)
	return nil
}

// pumpConverseStream drains src and writes each normalized event as a frame,
// in AWS order (messageStart -> per block contentBlockStart/Delta*/Stop ->
// messageStop -> metadata). A mid-stream Next error surfaces as an in-band
// exception event and stops the pump; a write/flush error also stops the
// pump silently, since the client connection is already broken and the
// HTTP status can no longer change either way.
func pumpConverseStream(ctx context.Context, fw *frameWriter, src converseStreamSource, modelID string) {
	for {
		event, ok, err := src.Next(ctx)
		if err != nil {
			excType := excInternalServerException
			var fault *streamFaultError
			if errors.As(err, &fault) {
				excType = excModelStreamErrorException
			}
			slog.Error("converse-stream: upstream fault", "model", modelID, "err", err)
			if werr := fw.writeException(excType, exceptionPayload(err)); werr != nil {
				slog.Error("converse-stream: failed to write exception frame", "model", modelID, "err", werr)
			}
			return
		}
		if !ok {
			return
		}

		payload, err := event.payload()
		if err != nil {
			slog.Error("converse-stream: failed to marshal event", "model", modelID, "kind", event.Kind, "err", err)
			if werr := fw.writeException(excInternalServerException, exceptionPayload(err)); werr != nil {
				slog.Error("converse-stream: failed to write exception frame", "model", modelID, "err", werr)
			}
			return
		}

		if werr := fw.writeEvent(event.Kind.eventType(), payload); werr != nil {
			slog.Error("converse-stream: failed to write frame, aborting", "model", modelID, "err", werr)
			return
		}
	}
}
