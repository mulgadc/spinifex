package ochre

import (
	"context"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"net/http"
	"sync"
	"time"

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

// defaultLivenessCacheTTL bounds how long a liveness re-probe of a persisted
// READY member is reused. Short, so a member that goes dark between probes
// (e.g. a co-resident engine still rebinding its port right after an awsgw
// restart) is caught within a couple of requests, but long enough above zero
// that resolving a warm, steady-state model costs one HTTP round trip per
// TTL rather than one per request.
const defaultLivenessCacheTTL = 2 * time.Second

// livenessProbeTimeout bounds a single liveness re-probe, so a member that
// accepts the connection but never answers cannot stall resolution.
const livenessProbeTimeout = 500 * time.Millisecond

// DynamicEndpointResolver resolves a self-host model's base URL through the
// daemon's endpoint registry, and asks for a launch when there is nothing to
// resolve.
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

	// client issues liveness re-probes against a persisted READY member's own
	// health route (see isLive). Not used for anything else here.
	client *http.Client
	// livenessTTL bounds how long a liveness probe result is reused; see
	// defaultLivenessCacheTTL.
	livenessTTL time.Duration

	livenessMu    sync.Mutex
	livenessCache map[string]livenessResult
}

// cachedEndpoint is one resolved READY base URL and the moment it expires.
type cachedEndpoint struct {
	baseURL   string
	expiresAt time.Time
}

// livenessOutcome is what one liveness re-probe of a member observed.
type livenessOutcome int

const (
	livenessNotLive livenessOutcome = iota
	livenessLive
	// livenessRedirected is a member answering its health route with a
	// redirect: it points elsewhere, so waiting on it cannot make it ready.
	livenessRedirected
)

// livenessResult is the outcome of the last liveness re-probe of one base
// URL and when it was taken.
type livenessResult struct {
	outcome   livenessOutcome
	checkedAt time.Time
}

var _ EndpointResolver = (*DynamicEndpointResolver)(nil)

// ResolverOption adjusts a DynamicEndpointResolver at construction.
type ResolverOption func(*DynamicEndpointResolver)

// WithColdStartWait holds a cold call for up to d waiting on the launch it
// triggered, instead of returning ModelNotReadyException straight away. The
// deployment-level escape hatch for a model known to start fast enough that
// waiting beats retrying; zero (the default) keeps the fail-fast contract.
func WithColdStartWait(d time.Duration) ResolverOption {
	return func(r *DynamicEndpointResolver) { r.coldStartWait = d }
}

// WithHTTPClient overrides the client isLive re-probes a READY member on.
// Production takes the default; tests point it at an httptest server.
func WithHTTPClient(c *http.Client) ResolverOption {
	return func(r *DynamicEndpointResolver) { r.client = c }
}

// WithLivenessCacheTTL overrides how long a liveness probe result is reused,
// so a test can exercise re-verification without waiting out the production
// interval.
func WithLivenessCacheTTL(d time.Duration) ResolverOption {
	return func(r *DynamicEndpointResolver) { r.livenessTTL = d }
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
		svc:           svc,
		static:        static,
		ttl:           ttl,
		cached:        make(map[string]cachedEndpoint),
		client:        &http.Client{},
		livenessTTL:   defaultLivenessCacheTTL,
		livenessCache: make(map[string]livenessResult),
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

var _ PinnedEndpointResolver = (*DynamicEndpointResolver)(nil)

// PinnedEndpoint returns modelID's base URL only from a static entry or a
// shared bundle that is READY, operator-pinned and live. It never launches,
// waits, or trusts the URL cache, which an unpinned resolve may have filled.
func (r *DynamicEndpointResolver) PinnedEndpoint(ctx context.Context, modelID string) (string, bool, error) {
	if baseURL, ok := r.static[modelID]; ok {
		return baseURL, true, nil
	}
	out, err := r.svc.Describe(ctx, &DescribeEndpointInput{ModelID: modelID, AccountID: awsidentifiers.GlobalAccountID},
		awsidentifiers.GlobalAccountID)
	if err != nil {
		return "", false, err
	}
	rec := out.Endpoint
	// A record without this member predates per-member ports, so its BaseURL
	// would name the bundle's generative engine rather than this model.
	if _, member := rec.Members[modelID]; rec.State != StateReady || !rec.Pinned || !rec.OperatorReserved || !member {
		return "", false, nil
	}
	url := rec.MemberBaseURL(modelID)
	if url == "" || !r.isLive(ctx, rec, modelID, url) {
		return "", false, nil
	}
	return url, true, nil
}

// resolve describes the endpoint and, when there is none, asks for one. An
// empty base URL means "not resolved", which is the only outcome a cold model
// can have: the launch outlives this request by design.
func (r *DynamicEndpointResolver) resolve(ctx context.Context, modelID string) (string, error) {
	baseURL, err := r.describeAndEnsure(ctx, awsidentifiers.GlobalAccountID, modelID, false)
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
		if url := out.Endpoint.MemberBaseURL(modelID); url != "" {
			switch r.liveness(ctx, out.Endpoint, modelID, url) {
			case livenessLive:
				return url, nil
			case livenessRedirected:
				return "", nil
			case livenessNotLive:
			}
			// The persisted record is READY but the member itself is not
			// answering (e.g. a co-resident engine still rebinding its port
			// right after an awsgw restart) -- nothing new to ensure, so
			// this is the same "wait or report not-ready" shape as a launch
			// already in flight, not a fresh launch request.
			return r.awaitReady(ctx, accountID, modelID)
		}
		return "", nil
	case StateStarting:
		// A launch is already in flight, so asking for another changes nothing.
		// Waiting on it is still worth doing when the deployment asked for that.
		return r.awaitReady(ctx, accountID, modelID)
	case StateDraining:
		// A teardown is in flight and nothing will relaunch on its own, so
		// there is no readiness to wait for.
		return "", nil
	case StateAbsent:
		// Nothing runs: ask for a launch below.
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
		if out.Endpoint.State == StateReady {
			if url := out.Endpoint.MemberBaseURL(modelID); url != "" {
				switch r.liveness(ctx, out.Endpoint, modelID, url) {
				case livenessLive:
					return url, nil
				case livenessRedirected:
					return "", nil
				case livenessNotLive:
				}
			}
		}
		if time.Now().After(deadline) {
			return "", nil
		}
	}
}

// isLive re-probes a persisted READY member's own readiness route before the
// resolver trusts it, short-TTL cached per base URL so a warm, steady-state
// model costs one HTTP round trip per livenessTTL rather than per resolve.
// A record with no per-member entry predates the Members field (or is a test
// fixture) and has no route to probe on, so it is trusted as before.
//
// This exists because a persisted READY state only means the endpoint was
// observed serving at some point in the past: after an awsgw restart the KV
// record survives untouched while a co-resident engine on the same VM is
// still rebinding its port, and describeAndEnsure has no other signal that
// the address it is about to hand out currently dials to nothing.
func (r *DynamicEndpointResolver) isLive(ctx context.Context, rec EndpointRecord, modelID, baseURL string) bool {
	return r.liveness(ctx, rec, modelID, baseURL) == livenessLive
}

// liveness is isLive's full outcome, cached the same way, so a caller can
// tell a redirected member from one that is merely not answering yet.
func (r *DynamicEndpointResolver) liveness(ctx context.Context, rec EndpointRecord, modelID, baseURL string) livenessOutcome {
	member, ok := rec.Members[modelID]
	if !ok {
		return livenessLive
	}

	r.livenessMu.Lock()
	if cached, ok := r.livenessCache[baseURL]; ok && time.Since(cached.checkedAt) < r.livenessTTL {
		r.livenessMu.Unlock()
		return cached.outcome
	}
	r.livenessMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, livenessProbeTimeout)
	defer cancel()
	outcome := probeLiveness(probeCtx, r.client, baseURL+readinessPath(member.Family))

	r.livenessMu.Lock()
	r.livenessCache[baseURL] = livenessResult{outcome: outcome, checkedAt: time.Now()}
	r.livenessMu.Unlock()
	return outcome
}

// probeLiveness GETs url through a shallow copy of client that hands any
// redirect back unfollowed, so client's transport, jar and timeout still
// apply and client itself is unchanged. Only HTTP 200 is live.
func probeLiveness(ctx context.Context, client *http.Client, url string) livenessOutcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return livenessNotLive
	}
	recorder := &redirectRecorder{next: client.Transport}
	probe := *client
	probe.Transport = recorder
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := probe.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	switch {
	case recorder.redirected:
		return livenessRedirected
	case err != nil:
		return livenessNotLive
	case resp.StatusCode == http.StatusOK:
		return livenessLive
	default:
		return livenessNotLive
	}
}

// redirectRecorder notes whether any response it carried was a redirect,
// including one whose Location the client then fails to parse.
type redirectRecorder struct {
	next       http.RoundTripper
	redirected bool
}

var _ http.RoundTripper = (*redirectRecorder)(nil)

func (r *redirectRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	next := r.next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(req)
	if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		r.redirected = true
	}
	return resp, err
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
