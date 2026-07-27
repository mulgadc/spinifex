package gateway_bedrock

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

// invokeStreamSource is the backend-agnostic InvokeModelWithResponseStream
// reframer contract: Next returns the next provider-native chunk (verbatim
// for Anthropic passthrough, translated to the Bedrock-native shape for
// Llama), or (nil,false,nil) at clean EOF.
type invokeStreamSource interface {
	Next(ctx context.Context) (chunk []byte, ok bool, err error)
	Close() error
}

// InvokeModelWithResponseStream is the bedrock-runtime
// InvokeModelWithResponseStream entry point used by the gateway route table.
// Unlike the JSON-dispatch handlers it owns w directly: a pre-stream failure
// (unknown model, unresolved credential, upstream connect error) returns an
// awserrors code for the normal ErrorHandler envelope. Once the first frame
// is written it always returns nil — any later failure is an in-band
// exception event, since the HTTP status can no longer change.
// requestContentType is the client's declared Content-Type, logged only.
func InvokeModelWithResponseStream(ctx context.Context, w http.ResponseWriter, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, requestContentType string) error {
	src, err := NewInvokeStreamRouter(resolver, endpointResolver).InvokeModelWithResponseStream(ctx, accountID, modelID, body)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); cerr != nil {
			slog.Error("invoke-with-response-stream: failed to close upstream source", "model", modelID, "err", cerr)
		}
	}()

	fw, err := newFrameWriter(w)
	if err != nil {
		return err
	}

	slog.Debug("invoke-with-response-stream: streaming", "model", modelID, "request_content_type", requestContentType)
	pumpInvokeStream(ctx, fw, src, modelID)
	return nil
}

// pumpInvokeStream drains src and writes each provider-native chunk as a
// "chunk" frame. A mid-stream Next error surfaces as an in-band exception
// event and stops the pump; a write/flush error also stops the pump
// silently, since the client connection is already broken and the HTTP
// status can no longer change either way.
func pumpInvokeStream(ctx context.Context, fw *frameWriter, src invokeStreamSource, modelID string) {
	for {
		chunk, ok, err := src.Next(ctx)
		if err != nil {
			excType := excInternalServerException
			var fault *streamFaultError
			if errors.As(err, &fault) {
				excType = excModelStreamErrorException
			}
			slog.Error("invoke-with-response-stream: upstream fault", "model", modelID, "err", err)
			if werr := fw.writeException(excType, exceptionPayload(err)); werr != nil {
				slog.Error("invoke-with-response-stream: failed to write exception frame", "model", modelID, "err", werr)
			}
			return
		}
		if !ok {
			return
		}

		if werr := fw.writeChunk(chunk); werr != nil {
			slog.Error("invoke-with-response-stream: failed to write frame, aborting", "model", modelID, "err", werr)
			return
		}
	}
}
