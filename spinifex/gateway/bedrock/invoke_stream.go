package gateway_bedrock

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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
// (unknown model, ungranted model, unresolved credential, upstream connect
// error) returns an awserrors code for the normal ErrorHandler envelope. Once
// the first frame is written it always returns nil — any later failure is an
// in-band exception event, since the HTTP status can no longer change.
// requestContentType is the client's declared Content-Type, logged only.
// resolver, endpointResolver, recorder and access may be nil;
// NewInvokeStreamRouter and the internal NoopRecorder fallback keep this call
// safe either way. provisioned may be nil, disabling PT ARN acceptance (any
// PT ARN then reads as an unknown modelId). guardrailIdent/guardrailVersion
// come from the request's X-Amzn-Bedrock-Guardrail* headers.
func InvokeModelWithResponseStream(ctx context.Context, w http.ResponseWriter, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, requestContentType string, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore, guardrailIdent, guardrailVersion string, guardrails *GuardrailStore) error {
	if recorder == nil {
		recorder = NoopRecorder
	}
	requestID := uuid.NewString()
	start := time.Now()

	// Resolved here too (InvokeStreamRouter below resolves it again for
	// routing) purely so entry's Provider tag reflects a PT ARN's target
	// model, not the raw ARN, for the InvocationRecord's Backend field.
	_, recordModelID, _ := resolveInferenceTarget(ctx, accountID, modelID, provisioned)
	entry, _ := lookupCatalogEntry(recordModelID) // InvokeStreamRouter below re-validates; only its Provider tag is needed here.

	src, err := NewInvokeStreamRouter(resolver, endpointResolver, access, provisioned, guardrails).InvokeModelWithResponseStream(ctx, accountID, modelID, body, guardrailIdent, guardrailVersion)
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
	pumpInvokeStream(ctx, fw, src, modelID, streamRecordCtx{
		recorder:  recorder,
		requestID: requestID,
		accountID: accountID,
		backend:   entry.Provider,
		operation: OperationInvokeModelWithResponseStream,
		start:     start,
		inputText: string(body),
	})
	return nil
}

// pumpInvokeStream drains src and writes each provider-native chunk as a
// "chunk" frame. A mid-stream Next error surfaces as an in-band exception
// event and stops the pump; a write/flush error also stops the pump
// silently, since the client connection is already broken and the HTTP
// status can no longer change either way. On every exit — clean end, client
// disconnect (ctx.Done), or upstream fault — the deferred closure records
// exactly one InvocationRecord via rc.recorder.
func pumpInvokeStream(ctx context.Context, fw *frameWriter, src invokeStreamSource, modelID string, rc streamRecordCtx) {
	if rc.recorder == nil {
		rc.recorder = NoopRecorder
	}
	partial := true
	errCode := ""

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
			// OutputText is intentionally left empty: chunks are opaque
			// provider-native bytes (Llama JSON or raw Anthropic SSE data),
			// with no generic way to recover assistant text across both.
		})
	}()

	for {
		select {
		case <-ctx.Done():
			errCode = errCodeClientDisconnected
			return
		default:
		}

		chunk, ok, err := src.Next(ctx)
		if err != nil {
			excType := excInternalServerException
			var fault *streamFaultError
			if errors.As(err, &fault) {
				excType = excModelStreamErrorException
			}
			errCode = awserrors.ValidErrorCodeFromError(err)
			slog.Error("invoke-with-response-stream: upstream fault", "model", modelID, "err", err)
			if werr := fw.writeException(excType, exceptionPayload(err)); werr != nil {
				slog.Error("invoke-with-response-stream: failed to write exception frame", "model", modelID, "err", werr)
				errCode = errCodeClientDisconnected
			}
			return
		}
		if !ok {
			partial = false
			return
		}

		if werr := fw.writeChunk(chunk); werr != nil {
			errCode = errCodeClientDisconnected
			slog.Error("invoke-with-response-stream: failed to write frame, aborting", "model", modelID, "err", werr)
			return
		}
	}
}
