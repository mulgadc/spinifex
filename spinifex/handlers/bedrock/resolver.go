package handlers_bedrock

import (
	"context"
	"sync"
	"time"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"golang.org/x/sync/singleflight"
)

// defaultEndpointCacheTTL bounds how long a READY base URL is reused without
// re-describing. Short, because the record can go away underneath the cache
// (a delete, or idle reclaim later), and the cost of being wrong is calls to
// an address that no longer answers.
const defaultEndpointCacheTTL = 5 * time.Second

// DynamicEndpointResolver resolves a self-host model's base URL through the
// daemon's endpoint registry, and asks for a launch when there is nothing to
// resolve. It lives here rather than in gateway_bedrock because that package
// cannot import this one: handlers_bedrock already imports it for
// LookupServingSpec.
type DynamicEndpointResolver struct {
	svc    EndpointService
	static map[string]string
	ttl    time.Duration

	// group collapses concurrent resolves of one model into a single describe
	// (and at most one ensure). The daemon deduplicates launches on its own
	// through the KV claim; this only stops a burst spending one NATS round
	// trip per request to learn the same answer.
	group singleflight.Group

	mu     sync.Mutex
	cached map[string]cachedEndpoint
}

// cachedEndpoint is one resolved READY base URL and the moment it expires.
type cachedEndpoint struct {
	baseURL   string
	expiresAt time.Time
}

var _ gateway_bedrock.EndpointResolver = (*DynamicEndpointResolver)(nil)

// NewDynamicEndpointResolver builds a resolver over svc. Entries in static
// (OCHRE_VLLM_ENDPOINTS) are resolved first and never reach svc, so a pinned
// endpoint bypasses the lifecycle entirely. A zero ttl takes the default.
func NewDynamicEndpointResolver(svc EndpointService, static map[string]string, ttl time.Duration) *DynamicEndpointResolver {
	if ttl <= 0 {
		ttl = defaultEndpointCacheTTL
	}
	return &DynamicEndpointResolver{
		svc:    svc,
		static: static,
		ttl:    ttl,
		cached: make(map[string]cachedEndpoint),
	}
}

// Endpoint returns modelID's base URL if one is serving. A model with no
// endpoint yet is requested from the daemon and reported as unresolved: the
// invoke paths turn that into ModelNotReadyException, so a cold call gets a
// retryable answer immediately and a retry once the VM is up succeeds.
func (r *DynamicEndpointResolver) Endpoint(ctx context.Context, modelID string) (string, bool, error) {
	if baseURL, ok := r.static[modelID]; ok {
		return baseURL, true, nil
	}
	if baseURL, ok := r.lookupCache(modelID); ok {
		return baseURL, true, nil
	}

	resolved, err, _ := r.group.Do(modelID, func() (any, error) {
		return r.resolve(ctx, modelID)
	})
	if err != nil {
		return "", false, err
	}
	baseURL, _ := resolved.(string)
	return baseURL, baseURL != "", nil
}

// resolve describes the endpoint and, when there is none, asks for one. An
// empty base URL means "not resolved", which is the only outcome a cold model
// can have: the launch outlives this request by design.
func (r *DynamicEndpointResolver) resolve(ctx context.Context, modelID string) (string, error) {
	out, err := r.svc.Describe(ctx, &DescribeEndpointInput{ModelID: modelID}, utils.GlobalAccountID)
	if err != nil {
		return "", err
	}

	switch out.Endpoint.State {
	case StateReady:
		if out.Endpoint.BaseURL == "" {
			return "", nil
		}
		r.storeCache(modelID, out.Endpoint.BaseURL)
		return out.Endpoint.BaseURL, nil
	case StateStarting, StateDraining:
		// A launch is already in flight, or a teardown is. Either way asking
		// again changes nothing and the answer is the same: not yet.
		return "", nil
	}

	if _, err := r.svc.Ensure(ctx, &EnsureEndpointInput{ModelID: modelID}, utils.GlobalAccountID); err != nil {
		return "", err
	}
	return "", nil
}

func (r *DynamicEndpointResolver) lookupCache(modelID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cached[modelID]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.baseURL, true
}

func (r *DynamicEndpointResolver) storeCache(modelID, baseURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached[modelID] = cachedEndpoint{baseURL: baseURL, expiresAt: time.Now().Add(r.ttl)}
}
