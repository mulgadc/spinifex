package gateway_bedrock

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/bedrockruntime"
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
}

// NewRouter constructs a Router. A nil resolver or endpointResolver falls
// back to a resolver/resolver that finds nothing, so a Router is always safe
// to use even before the real stores are wired in.
func NewRouter(resolver CredentialResolver, endpointResolver EndpointResolver) *Router {
	if resolver == nil {
		resolver = NoopCredentialResolver
	}
	if endpointResolver == nil {
		endpointResolver = NewStaticEndpointResolver(nil)
	}
	return &Router{resolver: resolver, endpointResolver: endpointResolver}
}

// Converse routes modelID to its provider via the catalog. Unknown modelIds
// and unresolvable vendors return ResourceNotFoundException; a vendor with no
// resolvable credential returns AccessDeniedException.
func (rt *Router) Converse(ctx context.Context, accountID, modelID string, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
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

	return p.Converse(ctx, modelID, input)
}

// Converse is the bedrock-runtime Converse entry point used by the gateway
// route table. resolver and endpointResolver may be nil; NewRouter supplies
// no-op fallbacks.
func Converse(ctx context.Context, accountID, modelID string, input *bedrockruntime.ConverseInput, resolver CredentialResolver, endpointResolver EndpointResolver) (*bedrockruntime.ConverseOutput, error) {
	return NewRouter(resolver, endpointResolver).Converse(ctx, accountID, modelID, input)
}
