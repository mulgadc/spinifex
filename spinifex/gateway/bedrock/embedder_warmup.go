package gateway_bedrock

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// embedderHealthPath is the readiness route embeddingsProvider's warm-up
// probe polls -- TEI has no other health route, mirroring
// handlers/bedrock/readiness.go's default (non-vLLM) family selection.
const embedderHealthPath = "/health"

// embedderWarmupPollInterval spaces the background warm-up probe's checks,
// and doubles as the positive-cache TTL: a ready result is trusted between
// probes, then re-verified next tick. Short enough that a cold-started TEI
// is picked up quickly, long enough not to hammer a healthy endpoint.
const embedderWarmupPollInterval = 2 * time.Second

// embedderWarmupProbeTimeout bounds a single warm-up health check, well
// under embedderWarmupPollInterval, so a slow or hung endpoint cannot stall
// the background loop's cadence.
const embedderWarmupProbeTimeout = 1 * time.Second

// warmupProbe tracks whether a configured static embedder endpoint is
// currently answering its health route, so Embed can fail fast with a clean
// "not ready" signal instead of dialing a port TEI hasn't bound yet. A
// background goroutine keeps the cached state fresh for the process
// lifetime and logs once when the endpoint first becomes ready, giving
// operators an "embedder ready" marker to bound the cold-start window from
// the awsgw side, independent of request traffic.
type warmupProbe struct {
	client  *http.Client
	baseURL string

	mu    sync.RWMutex
	ready bool
}

// newWarmupProbeWithInterval constructs a warmupProbe for baseURL and starts
// its background poll loop at pollInterval, then returns it. Production
// callers pass embedderWarmupPollInterval (see embeddingsProvider.warmupFor);
// tests pass a short interval to drive the loop without waiting out the
// production cadence.
func newWarmupProbeWithInterval(client *http.Client, baseURL string, pollInterval time.Duration) *warmupProbe {
	p := &warmupProbe{client: client, baseURL: baseURL}
	go p.run(pollInterval)
	return p
}

// run polls baseURL's health route every pollInterval for the life of the
// process, updating the cached ready state and logging exactly once on the
// not-ready -> ready transition.
func (p *warmupProbe) run(pollInterval time.Duration) {
	p.probe()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		p.probe()
	}
}

// probe issues one health check and updates the cached state, logging an
// "embedder ready" marker the first time it observes success.
func (p *warmupProbe) probe() {
	ok := probeEmbedderHealth(p.client, p.baseURL)

	p.mu.Lock()
	wasReady := p.ready
	p.ready = ok
	p.mu.Unlock()

	if ok && !wasReady {
		slog.Info("embedder ready", "service", embedServiceLabel, "action", "warmup_probe", "endpoint", p.baseURL)
	}
}

// Ready reports the most recently probed readiness state. It never blocks
// on network I/O -- the background loop in run does that -- so a caller on
// the request path gets an instant, if up-to-embedderWarmupPollInterval-
// stale, answer.
func (p *warmupProbe) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready
}

// probeEmbedderHealth issues a single bounded GET against baseURL+
// embedderHealthPath and reports whether it returned HTTP 200. Any
// transport error (connection refused while TEI is still starting) or
// non-200 status counts as not-yet-ready, mirroring
// handlers/bedrock/readiness.go's probeOnce -- duplicated rather than
// imported, since handlers_bedrock already imports gateway_bedrock and Go
// forbids the reverse.
func probeEmbedderHealth(client *http.Client, baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), embedderWarmupProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+embedderHealthPath, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
