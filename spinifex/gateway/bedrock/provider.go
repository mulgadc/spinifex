package gateway_bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// providerHTTPTimeout bounds outbound calls to both provider backends. It is
// long because Phase 1 is synchronous (no streaming) and matches the
// platform's tolerance for a single Converse call.
const providerHTTPTimeout = 15 * time.Minute

// Provider translates a Converse request into a backend's native wire format
// and back. vllmProvider serves self-hosted models over OpenAI chat
// completions; anthropicProvider (via newAnthropicProvider) serves Claude
// over the Anthropic Messages API with a per-call API key baked in.
type Provider interface {
	Converse(ctx context.Context, modelID string, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error)
}

// Router resolves a modelId to its catalog entry and dispatches to the
// matching Provider, resolving self-host endpoints and provider credentials
// as needed.
type Router struct {
	resolver         CredentialResolver
	endpointResolver EndpointResolver
	recorder         Recorder
}

// NewRouter constructs a Router. A nil resolver, endpointResolver, or
// recorder falls back to a no-op implementation, so a Router is always safe
// to use even before the real stores are wired in.
func NewRouter(resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder) *Router {
	if resolver == nil {
		resolver = NoopCredentialResolver
	}
	if endpointResolver == nil {
		endpointResolver = NewStaticEndpointResolver(nil)
	}
	if recorder == nil {
		recorder = NoopRecorder
	}
	return &Router{resolver: resolver, endpointResolver: endpointResolver, recorder: recorder}
}

// Converse routes modelID to its provider via the catalog. Unknown modelIds
// and unresolvable vendors return ResourceNotFoundException; a vendor with no
// resolvable credential returns AccessDeniedException. Every exit records an
// InvocationRecord via the deferred closure, matching pumpConverseStream's
// treatment of the streaming path.
func (rt *Router) Converse(ctx context.Context, accountID, modelID string, input *bedrockruntime.ConverseInput) (out *bedrockruntime.ConverseOutput, err error) {
	requestID := uuid.NewString()
	start := time.Now()
	var backend string
	defer func() {
		httpStatus, code := recordOutcome(err)
		var inputTokens, outputTokens int64
		if out != nil && out.Usage != nil {
			inputTokens = aws.Int64Value(out.Usage.InputTokens)
			outputTokens = aws.Int64Value(out.Usage.OutputTokens)
		}
		inputText, ierr := json.Marshal(input)
		if ierr != nil {
			slog.Error("bedrock converse: failed to marshal input for recording", "model", modelID, "err", ierr)
		}
		var outputText []byte
		if out != nil {
			var oerr error
			outputText, oerr = json.Marshal(out)
			if oerr != nil {
				slog.Error("bedrock converse: failed to marshal output for recording", "model", modelID, "err", oerr)
			}
		}
		rt.recorder.Record(ctx, InvocationRecord{
			RequestID:    requestID,
			AccountID:    accountID,
			ModelID:      modelID,
			Operation:    OperationConverse,
			Backend:      backend,
			LatencyMs:    time.Since(start).Milliseconds(),
			HTTPStatus:   httpStatus,
			ErrorCode:    code,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			InputText:    string(inputText),
			OutputText:   string(outputText),
		})
	}()

	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		err = errors.New(awserrors.ErrorResourceNotFoundException)
		return nil, err
	}
	backend = entry.Provider

	var p Provider
	switch {
	case entry.Provider == tierSelfHost:
		p = newVLLMProvider(rt.endpointResolver)
	case strings.HasPrefix(entry.Provider, providerPrefix):
		switch strings.TrimPrefix(entry.Provider, providerPrefix) {
		case vendorAnthropic:
			var key string
			var resolvable bool
			key, resolvable, err = rt.resolver.Resolve(ctx, accountID, vendorAnthropic)
			if err != nil {
				return nil, err
			}
			if !resolvable {
				err = errors.New(awserrors.ErrorAccessDeniedException)
				return nil, err
			}
			p = newAnthropicProvider(key)
		default:
			err = errors.New(awserrors.ErrorResourceNotFoundException)
			return nil, err
		}
	default:
		err = errors.New(awserrors.ErrorResourceNotFoundException)
		return nil, err
	}

	out, err = p.Converse(ctx, modelID, input)
	return out, err
}

// Converse is the bedrock-runtime Converse entry point used by the gateway
// route table. resolver, endpointResolver, and recorder may be nil; NewRouter
// supplies no-op fallbacks.
func Converse(ctx context.Context, accountID, modelID string, input *bedrockruntime.ConverseInput, resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder) (*bedrockruntime.ConverseOutput, error) {
	return NewRouter(resolver, endpointResolver, recorder).Converse(ctx, accountID, modelID, input)
}

// ConverseStreamProvider is the optional streaming capability a Provider may
// implement. Both shipped backends (vLLM, Anthropic) do; Router.ConverseStream
// type-asserts the resolved Provider to it rather than widening Provider
// itself, so a future non-streaming-capable backend still compiles.
type ConverseStreamProvider interface {
	ConverseStream(ctx context.Context, modelID string, input *bedrockruntime.ConverseStreamInput) (converseStreamSource, error)
}

// converseStreamToConverseInput adapts a ConverseStreamInput to a
// ConverseInput carrying only the fields buildVLLMRequest/buildAnthropicRequest
// read (Messages, System, InferenceConfig). The two generated types are
// otherwise structurally identical field-for-field; this lets the streaming
// path reuse the exact same request builders as Converse.
func converseStreamToConverseInput(input *bedrockruntime.ConverseStreamInput) *bedrockruntime.ConverseInput {
	return &bedrockruntime.ConverseInput{
		Messages:        input.Messages,
		System:          input.System,
		InferenceConfig: input.InferenceConfig,
	}
}

// ConverseStream routes modelID to its provider via the catalog, exactly like
// Converse, then requires the resolved provider to also implement
// ConverseStreamProvider.
func (rt *Router) ConverseStream(ctx context.Context, accountID, modelID string, input *bedrockruntime.ConverseStreamInput) (converseStreamSource, error) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	var p Provider
	switch {
	case entry.Provider == tierSelfHost:
		p = newVLLMProvider(rt.endpointResolver)
	case strings.HasPrefix(entry.Provider, providerPrefix):
		switch strings.TrimPrefix(entry.Provider, providerPrefix) {
		case vendorAnthropic:
			key, ok, err := rt.resolver.Resolve(ctx, accountID, vendorAnthropic)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errors.New(awserrors.ErrorAccessDeniedException)
			}
			p = newAnthropicProvider(key)
		default:
			return nil, errors.New(awserrors.ErrorResourceNotFoundException)
		}
	default:
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	sp, ok := p.(ConverseStreamProvider)
	if !ok {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	return sp.ConverseStream(ctx, modelID, input)
}
