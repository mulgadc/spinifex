package gateway_bedrock

import (
	"encoding/json"
	"errors"
	"net/http"

	esv2 "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// eventStreamContentType is the wire content type for both ConverseStream and
// InvokeModelWithResponseStream responses.
const eventStreamContentType = "application/vnd.amazon.eventstream"

// Bedrock in-band exception types, used as the frame ":exception-type"
// header once WriteHeader(200) has already been sent and the HTTP status can
// no longer change. Every pre-header failure instead returns a normal
// awserrors code for the ordinary JSON error envelope.
const (
	excInternalServerException   = "internalServerException"
	excModelStreamErrorException = "modelStreamErrorException"
)

// streamFaultError marks a reframer error as originating from the upstream
// connection (a broken/malformed provider stream) rather than our own
// internal encoding, so the pump surfaces it as modelStreamErrorException
// instead of the default internalServerException.
type streamFaultError struct{ err error }

// newStreamFault wraps err as a streamFaultError, or returns nil for a nil err.
func newStreamFault(err error) error {
	if err == nil {
		return nil
	}
	return &streamFaultError{err: err}
}

func (f *streamFaultError) Error() string { return f.err.Error() }
func (f *streamFaultError) Unwrap() error { return f.err }

// exceptionMessage is the payload shape for an in-band exception frame,
// matching the "message" field every generated Bedrock exception struct
// (InternalServerException, ModelStreamErrorException, ...) carries.
type exceptionMessage struct {
	Message string `json:"message"`
}

// exceptionPayload marshals err as an exception frame payload. Marshaling a
// single-string struct cannot fail; the fallback exists only to satisfy the
// type checker.
func exceptionPayload(err error) []byte {
	body, marshalErr := json.Marshal(exceptionMessage{Message: err.Error()})
	if marshalErr != nil {
		return []byte(`{"message":"internal error"}`)
	}
	return body
}

// flushSupported reports whether w (or anything it wraps via Unwrap, per the
// otelsetup statusRecorder fix) implements http.Flusher, without invoking
// Flush — a pure capability probe, so a non-Flusher writer can be rejected
// pre-header via the normal awserrors path rather than after WriteHeader(200)
// has already committed the response to 200.
func flushSupported(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

// frameWriter writes AWS event-stream frames to an HTTP response, flushing
// after each one so a streaming SDK consumer sees bytes incrementally rather
// than buffered until the handler returns. One frameWriter serves both
// ConverseStream and InvokeModelWithResponseStream.
type frameWriter struct {
	w    http.ResponseWriter
	enc  *esv2.Encoder
	ctrl *http.ResponseController
}

// newFrameWriter probes w for flush support (pre-header safe: flushSupported
// has no side effects on failure), then sets the event-stream content type
// and writes the 200 header. Once this returns successfully, no further
// failure may change the HTTP status — only in-band exception frames.
func newFrameWriter(w http.ResponseWriter) (*frameWriter, error) {
	if !flushSupported(w) {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	w.Header().Set("Content-Type", eventStreamContentType)
	w.WriteHeader(http.StatusOK)
	return &frameWriter{
		w:    w,
		enc:  esv2.NewEncoder(),
		ctrl: http.NewResponseController(w),
	}, nil
}

// writeMessage encodes headers+payload as one event-stream message and
// flushes it to the client.
func (fw *frameWriter) writeMessage(headers esv2.Headers, payload []byte) error {
	headers.Set(":content-type", esv2.StringValue("application/json"))
	if err := fw.enc.Encode(fw.w, esv2.Message{Headers: headers, Payload: payload}); err != nil {
		return err
	}
	return fw.ctrl.Flush()
}

// writeEvent frames payload as a normal ":message-type: event" frame of
// eventType (e.g. "messageStart", "chunk").
func (fw *frameWriter) writeEvent(eventType string, payload []byte) error {
	var headers esv2.Headers
	headers.Set(":message-type", esv2.StringValue("event"))
	headers.Set(":event-type", esv2.StringValue(eventType))
	return fw.writeMessage(headers, payload)
}

// writeException frames payload as an in-band ":message-type: exception"
// frame of excType. Used only after the 200 header has already been sent —
// the post-header error contract forbids changing the HTTP status.
func (fw *frameWriter) writeException(excType string, payload []byte) error {
	var headers esv2.Headers
	headers.Set(":message-type", esv2.StringValue("exception"))
	headers.Set(":exception-type", esv2.StringValue(excType))
	return fw.writeMessage(headers, payload)
}

// writeChunk frames a provider-native chunk as the
// InvokeModelWithResponseStream "chunk" event, base64-encoding nativeChunk
// into Bedrock's PayloadPart{Bytes} shape via the SDK's own restjson
// marshaler (jsonutil.BuildJSON), matching WriteJSONResponse's convention.
func (fw *frameWriter) writeChunk(nativeChunk []byte) error {
	payload, err := jsonutil.BuildJSON(&bedrockruntime.PayloadPart{Bytes: nativeChunk})
	if err != nil {
		return err
	}
	return fw.writeEvent("chunk", payload)
}
