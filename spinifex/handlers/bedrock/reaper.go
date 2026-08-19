package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// Sweep and lease timing. The holder refreshes well inside the bucket's TTL,
// so a leader that dies is replaced within one TTL rather than one refresh.
const (
	defaultReaperInterval = 30 * time.Second
	defaultLeaseRefresh   = 20 * time.Second

	// How long an endpoint must have been idle before its GPU is taken back.
	// Cold start was measured at 4m12s for the smallest model this platform
	// serves, so a TTL of a few multiples of that is what keeps a quiet-but-used
	// model from becoming a cold-start generator. Scale-to-zero is a cost
	// optimisation; a latency-sensitive workload buys warmth by pinning.
	defaultIdleTTL = 15 * time.Minute

	// How many consecutive failed scrapes mean the serving process is wedged
	// rather than briefly busy. Long enough that a GC pause or a dropped packet
	// is not an outage, short enough that a dead VM does not hold a device for a
	// whole idleTTL.
	maxScrapeFailures = 5

	// Per-scrape budget. Generous next to the sweep interval, because a slow
	// answer is still an answer and a timeout here counts as a failure.
	defaultScrapeTimeout = 5 * time.Second
)

// ReaperDeps are the reaper's overridable knobs. Every zero value takes the
// constant above; they exist so a test does not wait out production cadence.
type ReaperDeps struct {
	Interval      time.Duration
	IdleTTL       time.Duration
	LeaseRefresh  time.Duration
	ScrapeTimeout time.Duration
}

// Reaper is the leader-elected idle-reclaim loop. One node holds the lease and
// does the scraping and reaping; every node keeps serving the API, so a
// leaderless gap delays a reclaim rather than failing a request.
//
// It reads idleness from the serving process's own Prometheus /metrics rather
// than from gateway-reported activity: that keeps the daemon the single writer
// of endpoint state, survives a gateway node dying mid-call, and works
// unchanged with any number of gateways.
type Reaper struct {
	svc    *Service
	holder string
	deps   ReaperDeps

	mu     sync.Mutex
	leader bool
}

// NewReaper builds the reaper for svc; holder identifies this daemon in the lease.
func NewReaper(svc *Service, holder string, deps ReaperDeps) *Reaper {
	return &Reaper{svc: svc, holder: holder, deps: deps}
}

func (r *Reaper) interval() time.Duration {
	if r.deps.Interval > 0 {
		return r.deps.Interval
	}
	return defaultReaperInterval
}

func (r *Reaper) idleTTL() time.Duration {
	if r.deps.IdleTTL > 0 {
		return r.deps.IdleTTL
	}
	return defaultIdleTTL
}

func (r *Reaper) leaseRefresh() time.Duration {
	if r.deps.LeaseRefresh > 0 {
		return r.deps.LeaseRefresh
	}
	return defaultLeaseRefresh
}

func (r *Reaper) scrapeTimeout() time.Duration {
	if r.deps.ScrapeTimeout > 0 {
		return r.deps.ScrapeTimeout
	}
	return defaultScrapeTimeout
}

// Run drives the leadership and sweep loop until ctx is cancelled. Intended as
// a daemon-boot goroutine; panics are the caller's recover concern.
func (r *Reaper) Run(ctx context.Context) {
	leaseTicker := time.NewTicker(r.leaseRefresh())
	sweepTicker := time.NewTicker(r.interval())
	defer leaseTicker.Stop()
	defer sweepTicker.Stop()

	r.evaluateLeadership(ctx)
	for {
		select {
		case <-ctx.Done():
			r.relinquish()
			return
		case <-leaseTicker.C:
			r.evaluateLeadership(ctx)
		case <-sweepTicker.C:
			if !r.IsLeader() {
				continue
			}
			if err := r.sweepOnce(ctx); err != nil {
				slog.ErrorContext(ctx, "bedrock reaper: sweep failed", "holder", r.holder, "err", err)
			}
		}
	}
}

// IsLeader reports whether this node currently holds the reaper lease.
func (r *Reaper) IsLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leader
}

func (r *Reaper) evaluateLeadership(ctx context.Context) {
	won := r.acquireOrRefresh(ctx)
	r.mu.Lock()
	was := r.leader
	r.leader = won
	r.mu.Unlock()

	switch {
	case won && !was:
		slog.Info("bedrock reaper: elected leader", "holder", r.holder)
	case !won && was:
		slog.Info("bedrock reaper: lost leadership", "holder", r.holder)
	}
}

// acquireOrRefresh claims the lease, or refreshes it (resetting the TTL) when
// this node already holds it.
func (r *Reaper) acquireOrRefresh(ctx context.Context) bool {
	kv, err := r.leaderBucket(ctx)
	if err != nil {
		return false
	}
	if _, err := kv.Create(ctx, leaderKey, []byte(r.holder)); err == nil {
		return true
	}
	entry, err := kv.Get(ctx, leaderKey)
	if err != nil {
		return false
	}
	if string(entry.Value()) != r.holder {
		return false
	}
	if _, err := kv.Put(ctx, leaderKey, []byte(r.holder)); err != nil {
		return false
	}
	return true
}

// relinquish releases the lease on shutdown so the next leader is elected
// immediately rather than after the TTL.
func (r *Reaper) relinquish() {
	// Run's ctx is already cancelled by the time this is called, so the release
	// runs on its own — a cancelled ctx would fail the delete.
	ctx := context.Background()
	kv, err := r.leaderBucket(ctx)
	if err != nil {
		return
	}
	if entry, gerr := kv.Get(ctx, leaderKey); gerr == nil && string(entry.Value()) == r.holder {
		if err := kv.Delete(ctx, leaderKey); err != nil {
			slog.Debug("bedrock reaper: release lease failed", "holder", r.holder, "err", err)
		}
	}
}

func (r *Reaper) leaderBucket(ctx context.Context) (jetstream.KeyValue, error) {
	js, err := r.svc.js()
	if err != nil {
		return nil, err
	}
	return GetOrCreateLeaderBucket(ctx, js)
}

// sweepOnce scrapes every READY endpoint once and acts on what it saw. One
// endpoint's failure does not stop the pass: the others still need deciding.
func (r *Reaper) sweepOnce(ctx context.Context) error {
	kv, err := r.svc.bucket(ctx)
	if err != nil {
		return err
	}
	recs, err := ListEndpoints(ctx, kv, utils.GlobalAccountID)
	if err != nil {
		return err
	}
	var failures []error
	for _, rec := range recs {
		// STARTING and DRAINING belong to the launch goroutine and to Delete
		// respectively; both are mid-transition and neither is ours to judge.
		if rec.State != StateReady {
			continue
		}
		if err := r.sweepEndpoint(ctx, kv, rec); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", rec.ModelID, err))
		}
	}
	return errors.Join(failures...)
}

// reaperScrapesMetrics reports whether rec exposes the vLLM Prometheus series
// idle-reclaim reads. Only a bundle with a generative (vLLM) member does; a
// service-only bundle (embed/rerank) has no such load metrics and is
// liveness-probed instead. A record with no members recorded (one predating
// the Members field) is treated as vLLM, preserving the original behaviour.
func reaperScrapesMetrics(rec EndpointRecord) bool {
	if len(rec.Members) == 0 {
		return true
	}
	for _, m := range rec.Members {
		if m.Family == gateway_bedrock.FamilyMeta {
			return true
		}
	}
	return false
}

// sweepEndpoint decides one endpoint's fate from a single probe.
func (r *Reaper) sweepEndpoint(ctx context.Context, kv jetstream.KeyValue, rec EndpointRecord) error {
	scrapeCtx, cancel := context.WithTimeout(ctx, r.scrapeTimeout())
	defer cancel()

	// A bundle with no vLLM member exposes no load metrics to reclaim on, so
	// it is kept warm and only liveness-probed rather than idle-scraped.
	if !reaperScrapesMetrics(rec) {
		return r.sweepLiveness(scrapeCtx, kv, rec)
	}

	sample, err := scrapeMetrics(scrapeCtx, r.svc.httpClient(), rec.BaseURL)
	if err != nil {
		return r.recordScrapeFailure(ctx, kv, rec, err)
	}

	now := time.Now().UTC()
	updated := rec
	updated.ScrapeFailures = 0
	updated.InFlight = sample.inFlight()
	updated.SuccessTotal = sample.successTotal
	if !sample.idle(rec.SuccessTotal) {
		updated.LastActiveAt = now
	}

	if r.shouldReap(updated, now) {
		slog.InfoContext(ctx, "bedrock reaper: reclaiming an idle endpoint",
			"model", rec.ModelID, "instanceId", rec.InstanceID,
			"idle_ms", otelsetup.Millis(now.Sub(updated.LastActive())))
		if _, err := r.svc.Delete(ctx, &DeleteEndpointInput{ModelID: rec.ModelID}, utils.GlobalAccountID); err != nil {
			return fmt.Errorf("reap idle endpoint: %w", err)
		}
		return nil
	}
	return r.persistObservation(ctx, kv, rec, updated)
}

// sweepLiveness keeps a service-only bundle (embed/rerank, no generative
// member) up without idle-reclaim: it probes /health and only escalates a
// persistently unreachable, unpinned endpoint. A healthy probe refreshes the
// warm clock; there is deliberately no scale-to-zero for a bundle whose whole
// purpose is to stay warm on the retrieval hot path.
func (r *Reaper) sweepLiveness(ctx context.Context, kv jetstream.KeyValue, rec EndpointRecord) error {
	if !probeOnce(ctx, r.svc.httpClient(), rec.BaseURL+"/health") {
		return r.recordScrapeFailure(ctx, kv, rec, fmt.Errorf("liveness probe of %s/health failed", rec.BaseURL))
	}
	updated := rec
	updated.ScrapeFailures = 0
	updated.LastActiveAt = time.Now().UTC()
	return r.persistObservation(ctx, kv, rec, updated)
}

// shouldReap applies the two rules an idle endpoint must clear before its GPU
// is taken back: it must be idle for a full idleTTL, and it must have been
// READY for one. The second is not implied by the first — without it an
// endpoint could be reclaimed in the window between reaching READY and its
// first request landing, which is exactly the request that launched it.
func (r *Reaper) shouldReap(rec EndpointRecord, now time.Time) bool {
	if rec.Pinned || rec.InFlight > 0 {
		return false
	}
	ttl := r.idleTTL()
	return now.Sub(rec.LastActive()) >= ttl && now.Sub(rec.ReadyAt) >= ttl
}

// recordScrapeFailure counts an unknown sample and escalates a persistently
// unreachable endpoint to terminate-and-relaunch. Never treats the failure as
// idleness: a wedged VM must not be mistaken for a quiet one.
func (r *Reaper) recordScrapeFailure(ctx context.Context, kv jetstream.KeyValue, rec EndpointRecord, cause error) error {
	updated := rec
	updated.ScrapeFailures = rec.ScrapeFailures + 1

	// A pinned endpoint is operator- or commitment-managed: the reaper never
	// tears it down, exactly as shouldReap never idle-reclaims one. Keep
	// counting and logging so the failure stays visible, but never escalate.
	if rec.Pinned {
		slog.WarnContext(ctx, "bedrock reaper: scrape failed for a pinned endpoint; not reaping",
			"model", rec.ModelID, "instanceId", rec.InstanceID,
			"consecutiveFailures", updated.ScrapeFailures, "err", cause)
		return r.persistObservation(ctx, kv, rec, updated)
	}

	if updated.ScrapeFailures < maxScrapeFailures {
		slog.WarnContext(ctx, "bedrock reaper: metrics scrape failed",
			"model", rec.ModelID, "instanceId", rec.InstanceID,
			"consecutiveFailures", updated.ScrapeFailures, "err", cause)
		return r.persistObservation(ctx, kv, rec, updated)
	}

	slog.ErrorContext(ctx, "bedrock reaper: endpoint unreachable; terminating and relaunching",
		"model", rec.ModelID, "instanceId", rec.InstanceID,
		"consecutiveFailures", updated.ScrapeFailures, "err", cause)
	if _, err := r.svc.Delete(ctx, &DeleteEndpointInput{ModelID: rec.ModelID}, utils.GlobalAccountID); err != nil {
		return fmt.Errorf("terminate unreachable endpoint: %w", err)
	}
	// A relaunch that cannot be admitted (the device did not come back, or
	// another model took it) is not a sweep failure: the endpoint is gone, which
	// was the point, and the next invoke will ask for it again.
	if _, err := r.svc.Ensure(ctx, &EnsureEndpointInput{ModelID: rec.ModelID}, utils.GlobalAccountID); err != nil {
		slog.WarnContext(ctx, "bedrock reaper: relaunch of an unreachable endpoint was refused",
			"model", rec.ModelID, "err", err)
	}
	return nil
}

// persistObservation CAS-writes the sweep's findings, and only when they
// changed: a steady-state idle endpoint would otherwise cost a KV write per
// tick per endpoint forever.
func (r *Reaper) persistObservation(ctx context.Context, kv jetstream.KeyValue, prev, next EndpointRecord) error {
	if prev.LastActiveAt.Equal(next.LastActiveAt) && prev.InFlight == next.InFlight &&
		prev.SuccessTotal == next.SuccessTotal && prev.ScrapeFailures == next.ScrapeFailures {
		return nil
	}
	key := resolveKey(utils.GlobalAccountID, next.ModelID)
	current, rev, found, err := getFullJSON(ctx, kv, key)
	if err != nil {
		return err
	}
	if !found || current.Generation != prev.Generation || current.State != StateReady {
		// The record moved since this pass read it — a delete, or a relaunch that
		// reused the key. The next sweep re-reads rather than clobbering it.
		return nil
	}
	next.Generation = current.Generation + 1
	if err := updateJSON(ctx, kv, key, rev, next); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil
		}
		return err
	}
	return nil
}
