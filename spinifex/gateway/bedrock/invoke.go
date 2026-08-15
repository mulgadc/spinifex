package gateway_bedrock

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// resolveEndpointError maps a failed endpoint resolution onto the AWS code the
// daemon put on the wire, falling back to ServiceUnavailable for an un-coded
// transport failure.
func resolveEndpointError(err error) error {
	if code, ok := awserrors.ResolveErrorCode(err); ok {
		return errors.New(code)
	}
	return errors.New(awserrors.ErrorServiceUnavailableException)
}

// selfHostInvokeAdapter selects the InvokeAdapter for a self-host catalog
// entry by family: familyMeta serves through the Llama adapter (honouring a
// PT-account scope); any other family is refused rather than mis-served.
func selfHostInvokeAdapter(family string, endpointResolver EndpointResolver, ptAccountID string) (InvokeAdapter, error) {
	if family != familyMeta {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	if ptAccountID != "" {
		return newLlamaInvokeAdapterForAccount(endpointResolver, ptAccountID), nil
	}
	return newLlamaInvokeAdapter(endpointResolver), nil
}

// InvokeAdapter translates a Bedrock InvokeModel raw request body into a
// backend's native wire format and back, returning the response bytes
// verbatim with their content-type. Unlike Provider (Converse), the wire
// shape is per-family and is not unified by the gateway.
type InvokeAdapter interface {
	InvokeModel(ctx context.Context, modelID string, body []byte) (respBody []byte, contentType string, err error)
}

// InvokeRouter resolves a modelId to its catalog entry and dispatches to the
// matching InvokeAdapter, resolving self-host endpoints and provider
// credentials as needed.
type InvokeRouter struct {
	resolver         CredentialResolver
	endpointResolver EndpointResolver
	recorder         Recorder
	access           AccessResolver
	// provisioned resolves a provisioned-throughput ARN's commitment. Nil
	// means InvokeModel rejects any PT ARN as ResourceNotFoundException
	// rather than a bare modelId's usual denial.
	provisioned *ProvisionedStore
	// guardrails resolves an InvokeModel request's guardrail header. Nil
	// fails closed on any guardrail identifier (ResourceNotFoundException)
	// rather than proceeding unguarded; a request with none is unaffected.
	guardrails *GuardrailStore
}

// NewInvokeRouter constructs an InvokeRouter. A nil resolver, endpointResolver,
// or recorder falls back to a no-op implementation, and a nil access falls back
// to denying every model, so an InvokeRouter is always safe to use even before
// the real stores are wired in. A nil provisioned disables PT ARN acceptance.
func NewInvokeRouter(resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore, guardrails *GuardrailStore) *InvokeRouter {
	if resolver == nil {
		resolver = NoopCredentialResolver
	}
	if endpointResolver == nil {
		endpointResolver = NewStaticEndpointResolver(nil)
	}
	if recorder == nil {
		recorder = NoopRecorder
	}
	if access == nil {
		access = DenyAllAccessResolver
	}
	return &InvokeRouter{resolver: resolver, endpointResolver: endpointResolver, recorder: recorder, access: access, provisioned: provisioned, guardrails: guardrails}
}

// InvokeModel routes modelID to its family adapter via the catalog. Unknown
// modelIds and unresolvable vendors return ResourceNotFoundException; an
// ungranted model, or a vendor with no resolvable credential, returns
// AccessDeniedException. Every exit records an InvocationRecord via the
// deferred closure. guardrailIdent/guardrailVersion come from the request's
// X-Amzn-Bedrock-Guardrail* headers; an empty guardrailIdent leaves behaviour
// byte-identical to a request with no guardrail at all.
func (rt *InvokeRouter) InvokeModel(ctx context.Context, accountID, modelID string, body []byte, guardrailIdent, guardrailVersion string) (respBody []byte, contentType string, err error) {
	requestID := uuid.NewString()
	start := time.Now()
	var backend string
	defer func() {
		httpStatus, code := recordOutcome(err)
		inputTokens, outputTokens, _ := extractTokenUsage(backend, respBody)
		rt.recorder.Record(ctx, InvocationRecord{
			RequestID:    requestID,
			AccountID:    accountID,
			ModelID:      modelID,
			Operation:    OperationInvokeModel,
			Backend:      backend,
			LatencyMs:    time.Since(start).Milliseconds(),
			HTTPStatus:   httpStatus,
			ErrorCode:    code,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			InputText:    string(body),
			OutputText:   string(respBody),
		})
	}()

	// Translate before resolve: a PT ARN is swapped for the commitment's own
	// (account, foundation model) here, before catalog lookup, access grant,
	// and endpoint resolution all act on it. Assigns the named err, so the
	// deferred closure records a rejected PT ARN the same as any other
	// failed invocation.
	var ptAccountID string
	ptAccountID, modelID, err = resolveInferenceTarget(ctx, accountID, modelID, rt.provisioned)
	if err != nil {
		return nil, "", err
	}

	// Assigns the named err, so the deferred closure records a denied
	// invocation the same as any other failed one.
	entry, err := grantedCatalogEntry(ctx, accountID, modelID, rt.access)
	if err != nil {
		return nil, "", err
	}
	backend = entry.Provider

	var a InvokeAdapter
	switch {
	case entry.Provider == tierSelfHost:
		a, err = selfHostInvokeAdapter(entry.Family, rt.endpointResolver, ptAccountID)
		if err != nil {
			return nil, "", err
		}
	case strings.HasPrefix(entry.Provider, providerPrefix):
		switch strings.TrimPrefix(entry.Provider, providerPrefix) {
		case vendorAnthropic:
			var key string
			var resolvable bool
			key, resolvable, err = rt.resolver.Resolve(ctx, accountID, vendorAnthropic)
			if err != nil {
				return nil, "", err
			}
			if !resolvable {
				err = errors.New(awserrors.ErrorAccessDeniedException)
				return nil, "", err
			}
			a = newAnthropicInvokeAdapter(key)
		default:
			err = errors.New(awserrors.ErrorResourceNotFoundException)
			return nil, "", err
		}
	default:
		err = errors.New(awserrors.ErrorResourceNotFoundException)
		return nil, "", err
	}

	// INPUT guardrail check happens after the adapter is resolved (matching
	// Router.Converse), before the backend is ever called. Text extraction is
	// per-family, since the body is opaque/family-native at this layer.
	if guardrailIdent != "" {
		var texts []string
		var extractOK bool
		texts, extractOK = extractInvokePromptTexts(backend, body)
		if !extractOK {
			err = errors.New(awserrors.ErrorValidationException)
			return nil, "", err
		}
		var blocked bool
		var message string
		blocked, message, _, _, err = enforceGuardrail(ctx, rt.guardrails, accountID, guardrailIdent, guardrailVersion,
			bedrockruntime.GuardrailContentSourceInput, texts)
		if err != nil {
			return nil, "", err
		}
		if blocked {
			respBody, err = invokeGuardrailBlockedResponse(backend, modelID, message)
			if err != nil {
				return nil, "", err
			}
			return respBody, "application/json", nil
		}
	}

	respBody, contentType, err = a.InvokeModel(ctx, modelID, body)
	if err != nil || guardrailIdent == "" {
		return respBody, contentType, err
	}

	texts, extractOK := extractInvokeCompletionTexts(backend, respBody)
	if !extractOK {
		slog.Error("invoke: failed to extract completion text for guardrail OUTPUT check, blocking", "model", modelID, "backend", backend)
		view, verr := loadGuardrailView(ctx, rt.guardrails, accountID, guardrailIdent, guardrailVersion)
		if verr != nil {
			return nil, "", verr
		}
		respBody, err = invokeGuardrailBlockedResponse(backend, modelID, view.BlockedOutputsMessaging)
		if err != nil {
			return nil, "", err
		}
		return respBody, "application/json", nil
	}
	var blockedOut bool
	var messageOut string
	var redacted []string
	var gerr error
	blockedOut, messageOut, redacted, _, gerr = enforceGuardrail(ctx, rt.guardrails, accountID, guardrailIdent, guardrailVersion,
		bedrockruntime.GuardrailContentSourceOutput, texts)
	if gerr != nil {
		err = gerr
		return nil, "", err
	}
	if blockedOut {
		respBody, err = invokeGuardrailBlockedCompletion(backend, respBody, messageOut)
	} else {
		respBody, err = invokeGuardrailRedactedCompletion(backend, respBody, redacted)
	}
	if err != nil {
		return nil, "", err
	}
	return respBody, contentType, nil
}

// InvokeModel is the bedrock-runtime InvokeModel entry point used by the
// gateway route table. resolver, endpointResolver, recorder, access and
// provisioned may be nil; NewInvokeRouter supplies no-op (and, for access,
// deny-all; for provisioned, PT-ARN-rejecting) fallbacks. guardrailIdent/
// guardrailVersion come from the request's X-Amzn-Bedrock-Guardrail* headers.
func InvokeModel(ctx context.Context, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore, guardrailIdent, guardrailVersion string, guardrails *GuardrailStore) ([]byte, string, error) {
	return NewInvokeRouter(resolver, endpointResolver, recorder, access, provisioned, guardrails).InvokeModel(ctx, accountID, modelID, body, guardrailIdent, guardrailVersion)
}

// InvokeStreamAdapter is the optional streaming capability an InvokeAdapter
// may implement. Both shipped families (Llama/vLLM, Anthropic) do;
// InvokeStreamRouter type-asserts the resolved InvokeAdapter to it rather
// than widening InvokeAdapter itself, mirroring ConverseStreamProvider.
type InvokeStreamAdapter interface {
	InvokeModelWithResponseStream(ctx context.Context, modelID string, body []byte) (invokeStreamSource, error)
}

// InvokeStreamRouter resolves a modelId to its catalog entry and dispatches
// to the matching InvokeStreamAdapter, resolving self-host endpoints and
// provider credentials as needed.
type InvokeStreamRouter struct {
	resolver         CredentialResolver
	endpointResolver EndpointResolver
	access           AccessResolver
	// provisioned resolves a provisioned-throughput ARN's commitment. Nil
	// means InvokeModelWithResponseStream rejects any PT ARN as
	// ResourceNotFoundException, mirroring InvokeRouter.
	provisioned *ProvisionedStore
	// guardrails resolves an InvokeModelWithResponseStream request's
	// guardrail header, mirroring InvokeRouter.guardrails.
	guardrails *GuardrailStore
}

// NewInvokeStreamRouter constructs an InvokeStreamRouter. A nil resolver or
// endpointResolver falls back to a resolver/resolver that finds nothing, and a
// nil access falls back to denying every model, so an InvokeStreamRouter is
// always safe to use even before the real stores are wired in. A nil
// provisioned disables PT ARN acceptance.
func NewInvokeStreamRouter(resolver CredentialResolver, endpointResolver EndpointResolver, access AccessResolver, provisioned *ProvisionedStore, guardrails *GuardrailStore) *InvokeStreamRouter {
	if resolver == nil {
		resolver = NoopCredentialResolver
	}
	if endpointResolver == nil {
		endpointResolver = NewStaticEndpointResolver(nil)
	}
	if access == nil {
		access = DenyAllAccessResolver
	}
	return &InvokeStreamRouter{resolver: resolver, endpointResolver: endpointResolver, access: access, provisioned: provisioned, guardrails: guardrails}
}

// InvokeModelWithResponseStream routes modelID to its family adapter via the
// catalog, exactly like InvokeRouter.InvokeModel — including the same
// translate-before-resolve treatment of a PT ARN via rt.provisioned — then
// requires the resolved adapter to also implement InvokeStreamAdapter.
// guardrailIdent/guardrailVersion come from the request's
// X-Amzn-Bedrock-Guardrail* headers; an empty guardrailIdent leaves behaviour
// byte-identical to a request with no guardrail at all. An INPUT block never
// opens the backend stream; OUTPUT enforcement wraps the resolved source for
// buffered/SYNC assessment at end-of-stream.
func (rt *InvokeStreamRouter) InvokeModelWithResponseStream(ctx context.Context, accountID, modelID string, body []byte, guardrailIdent, guardrailVersion string) (invokeStreamSource, error) {
	// Translate before resolve, exactly like InvokeRouter.InvokeModel.
	ptAccountID, modelID, err := resolveInferenceTarget(ctx, accountID, modelID, rt.provisioned)
	if err != nil {
		return nil, err
	}

	entry, err := grantedCatalogEntry(ctx, accountID, modelID, rt.access)
	if err != nil {
		return nil, err
	}

	var a InvokeAdapter
	switch {
	case entry.Provider == tierSelfHost:
		a, err = selfHostInvokeAdapter(entry.Family, rt.endpointResolver, ptAccountID)
		if err != nil {
			return nil, err
		}
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
			a = newAnthropicInvokeAdapter(key)
		default:
			return nil, errors.New(awserrors.ErrorResourceNotFoundException)
		}
	default:
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	sa, ok := a.(InvokeStreamAdapter)
	if !ok {
		return nil, errors.New(awserrors.ErrorValidationException)
	}

	if guardrailIdent != "" {
		texts, extractOK := extractInvokePromptTexts(entry.Provider, body)
		if !extractOK {
			return nil, errors.New(awserrors.ErrorValidationException)
		}
		blocked, message, _, _, gerr := enforceGuardrail(ctx, rt.guardrails, accountID, guardrailIdent, guardrailVersion,
			bedrockruntime.GuardrailContentSourceInput, texts)
		if gerr != nil {
			return nil, gerr
		}
		if blocked {
			chunks, berr := buildGuardedInvokeStreamChunks(entry.Provider, message, true)
			if berr != nil {
				return nil, berr
			}
			return &blockedInvokeStreamSource{chunks: chunks}, nil
		}
	}

	src, err := sa.InvokeModelWithResponseStream(ctx, modelID, body)
	if err != nil {
		return nil, err
	}
	if guardrailIdent != "" {
		src = newGuardrailInvokeStreamSource(src, rt.guardrails, accountID, guardrailIdent, guardrailVersion, entry.Provider)
	}
	return src, nil
}
