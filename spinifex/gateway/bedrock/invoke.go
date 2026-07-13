package gateway_bedrock

import (
	"context"
	"errors"
	"strings"

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
}

// NewInvokeRouter constructs an InvokeRouter. A nil resolver or
// endpointResolver falls back to a resolver/resolver that finds nothing, so
// an InvokeRouter is always safe to use even before the real stores are
// wired in.
func NewInvokeRouter(resolver CredentialResolver, endpointResolver EndpointResolver) *InvokeRouter {
	if resolver == nil {
		resolver = NoopCredentialResolver
	}
	if endpointResolver == nil {
		endpointResolver = NewStaticEndpointResolver(nil)
	}
	return &InvokeRouter{resolver: resolver, endpointResolver: endpointResolver}
}

// InvokeModel routes modelID to its family adapter via the catalog. Unknown
// modelIds and unresolvable vendors return ResourceNotFoundException; a
// vendor with no resolvable credential returns AccessDeniedException.
func (rt *InvokeRouter) InvokeModel(ctx context.Context, accountID, modelID string, body []byte) ([]byte, string, error) {
	entry, ok := lookupCatalogEntry(modelID)
	if !ok {
		return nil, "", errors.New(awserrors.ErrorResourceNotFoundException)
	}

	var a InvokeAdapter
	switch {
	case entry.Provider == tierSelfHost:
		a = newLlamaInvokeAdapter(rt.endpointResolver)
	case strings.HasPrefix(entry.Provider, providerPrefix):
		switch strings.TrimPrefix(entry.Provider, providerPrefix) {
		case vendorAnthropic:
			key, ok, err := rt.resolver.Resolve(ctx, accountID, vendorAnthropic)
			if err != nil {
				return nil, "", err
			}
			if !ok {
				return nil, "", errors.New(awserrors.ErrorAccessDeniedException)
			}
			a = newAnthropicInvokeAdapter(key)
		default:
			return nil, "", errors.New(awserrors.ErrorResourceNotFoundException)
		}
	default:
		return nil, "", errors.New(awserrors.ErrorResourceNotFoundException)
	}

	return a.InvokeModel(ctx, modelID, body)
}

// InvokeModel is the bedrock-runtime InvokeModel entry point used by the
// gateway route table. resolver and endpointResolver may be nil;
// NewInvokeRouter supplies no-op fallbacks.
func InvokeModel(ctx context.Context, accountID, modelID string, body []byte, resolver CredentialResolver, endpointResolver EndpointResolver) ([]byte, string, error) {
	return NewInvokeRouter(resolver, endpointResolver).InvokeModel(ctx, accountID, modelID, body)
}
