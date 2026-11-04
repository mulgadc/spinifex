package ochre

import (
	"context"
	"log/slog"
	"net/http"
	"time"
	"uuid"
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
// NewRouter and the internal NoopRecorder fallback keep this call
// safe either way. provisioned may be nil, disabling PT ARN acceptance (any
// PT ARN then reads as an unknown modelId). guardrailIdent/guardrailVersion
// come from the request's X-Amzn-Bedrock-Guardrail* headers. embedder drives
// topicPolicy's semantic match; nil falls back to the literal matcher.
func InvokeModelWithResponseStream(ctx context.Context, w http.ResponseWriter, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, requestContentType string, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore, guardrailIdent, guardrailVersion string, guardrails *GuardrailStore, embedder Embedder) error {
	if recorder == nil {
		recorder = NoopRecorder
	}
	requestID := uuid.NewV4().String()
	start := time.Now()

	// Resolved here too (the Router below resolves it again for
	// routing) purely so entry's Provider tag reflects a PT ARN's target
	// model, not the raw ARN, for the InvocationRecord's Backend field.
	_, recordModelID, _ := resolveInferenceTarget(ctx, accountID, modelID, provisioned)
	entry, _ := lookupCatalogEntry(recordModelID) // the Router below re-validates; only its Provider tag is needed here.

	src, err := NewRouter(resolver, endpointResolver, nil, access, provisioned, guardrails, embedder).InvokeModelWithResponseStream(ctx, accountID, modelID, body, guardrailIdent, guardrailVersion)
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
	partial := true
	errCode := ""
	// OutputText stays empty: chunks are opaque provider-native bytes with no
	// generic way to recover assistant text across Llama and Anthropic.
	defer func() { rc.record(ctx, src, modelID, errCode, partial, "") }()

	for {
		select {
		case <-ctx.Done():
			errCode = errCodeClientDisconnected
			return
		default:
		}

		chunk, ok, err := src.Next(ctx)
		if err != nil {
			errCode = fw.writeUpstreamFault("invoke-with-response-stream", modelID, err)
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
