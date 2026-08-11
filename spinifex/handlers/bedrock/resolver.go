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

// coldStartPollInterval spaces the describes a non-zero coldStartWait makes.
// Fine-grained next to any plausible wait, since the point of waiting at all
// is to return the moment the endpoint answers.
const coldStartPollInterval = 500 * time.Millisecond

// DynamicEndpointResolver resolves a self-host model's base URL through the
// daemon's endpoint registry, and asks for a launch when there is nothing to
// resolve. It lives here rather than in gateway_bedrock because that package
// cannot import this one: handlers_bedrock already imports it for
// LookupServingSpec.
type DynamicEndpointResolver struct {
	svc    EndpointService
	static map[string]string
	ttl    time.Duration

	// coldStartWait is how long a cold call may be held waiting for the launch
	// it triggered. Zero — the default — returns immediately, which is the
	// measured position: cold start is minutes, the AWS SDKs' default ~3-attempt
	// retry does not span that, and a bounded wait only pays off when it is
	// shorter than the client's patience and longer than the boot.
	coldStartWait time.Duration

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

// ResolverOption adjusts a DynamicEndpointResolver at construction.
type ResolverOption func(*DynamicEndpointResolver)

// WithColdStartWait holds a cold call for up to d waiting on the launch it
// triggered, instead of returning ModelNotReadyException straight away. The
// deployment-level escape hatch for a model known to start fast enough that
// waiting beats retrying; zero (the default) keeps the fail-fast contract.
func WithColdStartWait(d time.Duration) ResolverOption {
	return func(r *DynamicEndpointResolver) { r.coldStartWait = d }
}

// NewDynamicEndpointResolver builds a resolver over svc. Entries in static
// (OCHRE_VLLM_ENDPOINTS) are resolved first and never reach svc, so a pinned
// endpoint bypasses the lifecycle entirely. A zero ttl takes the default.
func NewDynamicEndpointResolver(svc EndpointService, static map[string]string, ttl time.Duration,
	opts ...ResolverOption) *DynamicEndpointResolver {
	if ttl <= 0 {
		ttl = defaultEndpointCacheTTL
	}
	r := &DynamicEndpointResolver{
		svc:    svc,
		static: static,
		ttl:    ttl,
		cached: make(map[string]cachedEndpoint),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
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
	baseURL, err := r.describeAndEnsure(ctx, utils.GlobalAccountID, modelID, false)
	if err != nil || baseURL == "" {
		return baseURL, err
	}
	r.storeCache(modelID, baseURL)
	return baseURL, nil
}

// EndpointForAccount resolves modelID's base URL scoped to accountID rather
// than the GlobalAccountID shorthand Endpoint/resolve use — the
// provisioned-throughput path, where the pinned endpoint is keyed to the
// commitment's own account. It bypasses the static map and TTL cache
// entirely: both are keyed by modelID alone for the shared shorthand, and
// caching an account-scoped answer under that same key would leak a pinned
// endpoint's address to (or steal a shared one from) an unrelated account.
// A re-Ensure here keeps Pinned:true, mirroring how the commitment was
// created.
func (r *DynamicEndpointResolver) EndpointForAccount(ctx context.Context, accountID, modelID string) (string, bool, error) {
	baseURL, err := r.describeAndEnsure(ctx, accountID, modelID, true)
	if err != nil {
		return "", false, err
	}
	return baseURL, baseURL != "", nil
}

// describeAndEnsure describes (accountID, modelID) and, when it is not
// currently serving, requests one (pinned when pinned is true), then waits
// up to coldStartWait for readiness. Shared by the GlobalAccountID shorthand
// (resolve, which caches its own answer) and the account-aware PT path
// (EndpointForAccount, which does not).
func (r *DynamicEndpointResolver) describeAndEnsure(ctx context.Context, accountID, modelID string, pinned bool) (string, error) {
	out, err := r.svc.Describe(ctx, &DescribeEndpointInput{ModelID: modelID, AccountID: accountID}, accountID)
	if err != nil {
		return "", err
	}

	switch out.Endpoint.State {
	case StateReady:
		if out.Endpoint.BaseURL == "" {
			return "", nil
		}
		return out.Endpoint.BaseURL, nil
	case StateStarting:
		// A launch is already in flight, so asking for another changes nothing.
		// Waiting on it is still worth doing when the deployment asked for that.
		return r.awaitReady(ctx, accountID, modelID)
	case StateDraining:
		// A teardown is in flight and nothing will relaunch on its own, so
		// there is no readiness to wait for.
		return "", nil
	}

	if _, err := r.svc.Ensure(ctx, &EnsureEndpointInput{ModelID: modelID, AccountID: accountID, Pinned: pinned}, accountID); err != nil {
		return "", err
	}
	return r.awaitReady(ctx, accountID, modelID)
}

// awaitReady holds the caller for up to coldStartWait waiting on a launch,
// and returns "" (not ready) the moment the budget runs out. At the default
// of zero it does nothing at all, so the fail-fast path costs no extra
// describe. For the GlobalAccountID shorthand it runs inside the
// singleflight, so a burst of concurrent callers for one cold model shares a
// single wait rather than one describe loop each; the account-aware path has
// no such burst protection, matching describeAndEnsure's no-cache stance.
func (r *DynamicEndpointResolver) awaitReady(ctx context.Context, accountID, modelID string) (string, error) {
	if r.coldStartWait <= 0 {
		return "", nil
	}
	deadline := time.Now().Add(r.coldStartWait)
	ticker := time.NewTicker(coldStartPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The caller is gone. The launch it triggered carries on regardless,
			// so this is not-ready rather than an error.
			return "", nil
		case <-ticker.C:
		}
		out, err := r.svc.Describe(ctx, &DescribeEndpointInput{ModelID: modelID, AccountID: accountID}, accountID)
		if err != nil {
			return "", err
		}
		if out.Endpoint.State == StateReady && out.Endpoint.BaseURL != "" {
			return out.Endpoint.BaseURL, nil
		}
		if time.Now().After(deadline) {
			return "", nil
		}
	}
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
