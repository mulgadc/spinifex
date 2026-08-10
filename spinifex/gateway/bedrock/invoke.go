package gateway_bedrock

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

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
}

// NewInvokeRouter constructs an InvokeRouter. A nil resolver, endpointResolver,
// or recorder falls back to a no-op implementation, and a nil access falls back
// to denying every model, so an InvokeRouter is always safe to use even before
// the real stores are wired in. A nil provisioned disables PT ARN acceptance.
func NewInvokeRouter(resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore) *InvokeRouter {
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
	return &InvokeRouter{resolver: resolver, endpointResolver: endpointResolver, recorder: recorder, access: access, provisioned: provisioned}
}

// InvokeModel routes modelID to its family adapter via the catalog. Unknown
// modelIds and unresolvable vendors return ResourceNotFoundException; an
// ungranted model, or a vendor with no resolvable credential, returns
// AccessDeniedException. Every exit records an InvocationRecord via the
// deferred closure.
func (rt *InvokeRouter) InvokeModel(ctx context.Context, accountID, modelID string, body []byte) (respBody []byte, contentType string, err error) {
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
		if ptAccountID != "" {
			a = newLlamaInvokeAdapterForAccount(rt.endpointResolver, ptAccountID)
		} else {
			a = newLlamaInvokeAdapter(rt.endpointResolver)
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

	respBody, contentType, err = a.InvokeModel(ctx, modelID, body)
	return respBody, contentType, err
}

// InvokeModel is the bedrock-runtime InvokeModel entry point used by the
// gateway route table. resolver, endpointResolver, recorder, access and
// provisioned may be nil; NewInvokeRouter supplies no-op (and, for access,
// deny-all; for provisioned, PT-ARN-rejecting) fallbacks.
func InvokeModel(ctx context.Context, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver, recorder Recorder, access AccessResolver, provisioned *ProvisionedStore) ([]byte, string, error) {
	return NewInvokeRouter(resolver, endpointResolver, recorder, access, provisioned).InvokeModel(ctx, accountID, modelID, body)
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
}

// NewInvokeStreamRouter constructs an InvokeStreamRouter. A nil resolver or
// endpointResolver falls back to a resolver/resolver that finds nothing, and a
// nil access falls back to denying every model, so an InvokeStreamRouter is
// always safe to use even before the real stores are wired in. A nil
// provisioned disables PT ARN acceptance.
func NewInvokeStreamRouter(resolver CredentialResolver, endpointResolver EndpointResolver, access AccessResolver, provisioned *ProvisionedStore) *InvokeStreamRouter {
	if resolver == nil {
		resolver = NoopCredentialResolver
	}
	if endpointResolver == nil {
		endpointResolver = NewStaticEndpointResolver(nil)
	}
	if access == nil {
		access = DenyAllAccessResolver
	}
	return &InvokeStreamRouter{resolver: resolver, endpointResolver: endpointResolver, access: access, provisioned: provisioned}
}

// InvokeModelWithResponseStream routes modelID to its family adapter via the
// catalog, exactly like InvokeRouter.InvokeModel — including the same
// translate-before-resolve treatment of a PT ARN via rt.provisioned — then
// requires the resolved adapter to also implement InvokeStreamAdapter.
func (rt *InvokeStreamRouter) InvokeModelWithResponseStream(ctx context.Context, accountID, modelID string, body []byte) (invokeStreamSource, error) {
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
		if ptAccountID != "" {
			a = newLlamaInvokeAdapterForAccount(rt.endpointResolver, ptAccountID)
		} else {
			a = newLlamaInvokeAdapter(rt.endpointResolver)
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
	return sa.InvokeModelWithResponseStream(ctx, modelID, body)
}
