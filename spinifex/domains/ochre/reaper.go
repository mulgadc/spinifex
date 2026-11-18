package ochre

import (
	"context"
	"errors"
	"fmt"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// KeyReaper is the reaper's leader lease in the sync bucket.
const KeyReaper = "ochre.reaper"

// Sweep and lease timing. A leader that dies is replaced within about one TTL.
const (
	reaperLeaseTTL        = 60 * time.Second
	defaultReaperInterval = 30 * time.Second
	defaultIdleTTL        = 15 * time.Minute // How long an endpoint must have been idle before its GPU is taken back.
	maxScrapeFailures     = 5                // How many consecutive failed scrapes mean the serving process is wedged rather than briefly busy.
	defaultScrapeTimeout  = 5 * time.Second  // Per-scrape budget.
)

// ReaperDeps are the reaper's overridable knobs. Every zero value takes the
// constant above; they exist so a test does not wait out production cadence.
type ReaperDeps struct {
	Interval      time.Duration
	IdleTTL       time.Duration
	ScrapeTimeout time.Duration
}

// Reaper is the leader-elected idle-reclaim loop. One node holds the lease and
// does the scraping and reaping; every node keeps serving the API, so a
// leaderless gap delays a reclaim rather than failing a request.
type Reaper struct {
	svc   *Service
	deps  ReaperDeps
	lease *kvsync.Lease
}

// NewReaperLease returns the reaper's leader lease for holder.
func NewReaperLease(store kvsync.KVStore, holder string) *kvsync.Lease {
	return kvsync.NewLease(store, KeyReaper, holder, reaperLeaseTTL)
}

// NewReaper builds the reaper for svc, which leads while it holds lease.
func NewReaper(svc *Service, lease *kvsync.Lease, deps ReaperDeps) *Reaper {
	return &Reaper{svc: svc, deps: deps, lease: lease}
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

func (r *Reaper) scrapeTimeout() time.Duration {
	if r.deps.ScrapeTimeout > 0 {
		return r.deps.ScrapeTimeout
	}
	return defaultScrapeTimeout
}

// Run drives the leadership and sweep loop until ctx is cancelled. Intended as
// a daemon-boot goroutine; panics are the caller's recover concern.
func (r *Reaper) Run(ctx context.Context) {
	sweepTicker := time.NewTicker(r.interval())
	defer sweepTicker.Stop()

	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		kvsync.RunElection(ctx, r.lease, nil, nil)
	}()

	for {
		select {
		case <-ctx.Done():
			<-leaseDone
			return
		case <-sweepTicker.C:
			r.tick(ctx)
		}
	}
}

// tick sweeps once if this node leads. Lost is read before Held, so a lease
// gained in between yields the previous, closed channel and the sweep acts on
// nothing; a lease lost after the check closes the channel the sweep holds.
func (r *Reaper) tick(ctx context.Context) {
	lost := r.lease.Lost()
	if !r.IsLeader() {
		return
	}
	if err := r.sweepOnce(ctx, lost); err != nil {
		slog.ErrorContext(ctx, "bedrock reaper: sweep failed", "key", r.lease.Key(), "err", err)
	}
}

// leading reports whether the acquisition whose Lost channel is lost still
// holds. A nil lost never closes.
func leading(lost <-chan struct{}) bool {
	select {
	case <-lost:
		return false
	default:
		return true
	}
}

// IsLeader reports whether this node currently holds the reaper lease.
func (r *Reaper) IsLeader() bool {
	return r.lease.Held()
}

// sweepOnce scrapes every READY endpoint once and acts on what it saw, while
// the acquisition lost belongs to holds. One endpoint's failure does not stop
// the pass: the others still need deciding.
func (r *Reaper) sweepOnce(ctx context.Context, lost <-chan struct{}) error {
	recs, err := r.svc.store.list(ctx, awsidentifiers.GlobalAccountID)
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
		if !leading(lost) {
			slog.InfoContext(ctx, "bedrock reaper: lease lost; ending the sweep", "key", r.lease.Key())
			break
		}
		if err := r.sweepEndpoint(ctx, rec, lost); err != nil {
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
		if m.Family == FamilyMeta {
			return true
		}
	}
	return false
}

// sweepEndpoint decides one endpoint's fate from a single probe.
func (r *Reaper) sweepEndpoint(ctx context.Context, rec EndpointRecord, lost <-chan struct{}) error {
	scrapeCtx, cancel := context.WithTimeout(ctx, r.scrapeTimeout())
	defer cancel()

	// A bundle with no vLLM member exposes no load metrics to reclaim on, so
	// it is kept warm and only liveness-probed rather than idle-scraped.
	if !reaperScrapesMetrics(rec) {
		return r.sweepLiveness(scrapeCtx, rec, lost)
	}

	sample, err := scrapeMetrics(scrapeCtx, r.svc.httpClient(), rec.BaseURL)
	if err != nil {
		return r.recordScrapeFailure(ctx, rec, err, lost)
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
		if _, err := r.reclaim(ctx, rec, lost); err != nil {
			return fmt.Errorf("reap idle endpoint: %w", err)
		}
		return nil
	}
	return r.persistObservation(ctx, rec, updated)
}

// sweepLiveness keeps a service-only bundle (embed/rerank, no generative
// member) up without idle-reclaim: it probes /health and only escalates a
// persistently unreachable, unpinned endpoint. A healthy probe refreshes the
// warm clock; there is deliberately no scale-to-zero for a bundle whose whole
// purpose is to stay warm on the retrieval hot path.
func (r *Reaper) sweepLiveness(ctx context.Context, rec EndpointRecord, lost <-chan struct{}) error {
	if !probeOnce(ctx, r.svc.httpClient(), rec.BaseURL+"/health") {
		return r.recordScrapeFailure(ctx, rec, fmt.Errorf("liveness probe of %s/health failed", rec.BaseURL), lost)
	}
	updated := rec
	updated.ScrapeFailures = 0
	updated.LastActiveAt = time.Now().UTC()
	return r.persistObservation(ctx, rec, updated)
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
func (r *Reaper) recordScrapeFailure(ctx context.Context, rec EndpointRecord, cause error, lost <-chan struct{}) error {
	updated := rec
	updated.ScrapeFailures = rec.ScrapeFailures + 1

	// A pinned endpoint is operator- or commitment-managed: the reaper never
	// tears it down, exactly as shouldReap never idle-reclaims one. Keep
	// counting and logging so the failure stays visible, but never escalate.
	if rec.Pinned {
		slog.WarnContext(ctx, "bedrock reaper: scrape failed for a pinned endpoint; not reaping",
			"model", rec.ModelID, "instanceId", rec.InstanceID,
			"consecutiveFailures", updated.ScrapeFailures, "err", cause)
		return r.persistObservation(ctx, rec, updated)
	}

	if updated.ScrapeFailures < maxScrapeFailures {
		slog.WarnContext(ctx, "bedrock reaper: metrics scrape failed",
			"model", rec.ModelID, "instanceId", rec.InstanceID,
			"consecutiveFailures", updated.ScrapeFailures, "err", cause)
		return r.persistObservation(ctx, rec, updated)
	}

	slog.ErrorContext(ctx, "bedrock reaper: endpoint unreachable; terminating and relaunching",
		"model", rec.ModelID, "instanceId", rec.InstanceID,
		"consecutiveFailures", updated.ScrapeFailures, "err", cause)
	reclaimed, err := r.reclaim(ctx, rec, lost)
	if err != nil {
		return fmt.Errorf("terminate unreachable endpoint: %w", err)
	}
	if !reclaimed || !leading(lost) {
		return nil
	}
	// A relaunch that cannot be admitted (the device did not come back, or
	// another model took it) is not a sweep failure: the endpoint is gone, which
	// was the point, and the next invoke will ask for it again.
	if _, err := r.svc.Ensure(ctx, &EnsureEndpointInput{ModelID: rec.ModelID}, awsidentifiers.GlobalAccountID); err != nil {
		slog.WarnContext(ctx, "bedrock reaper: relaunch of an unreachable endpoint was refused",
			"model", rec.ModelID, "err", err)
	}
	return nil
}

// reclaim tears rec down if it is still the record this sweep read and lost
// has not closed, reporting whether it did. A refusal leaves the endpoint to
// whoever changed it or now leads, so it is not a failed sweep.
func (r *Reaper) reclaim(ctx context.Context, rec EndpointRecord, lost <-chan struct{}) (bool, error) {
	err := r.svc.reclaim(ctx, rec, func() bool { return leading(lost) })
	if errors.Is(err, errReclaimSuperseded) {
		slog.InfoContext(ctx, "bedrock reaper: endpoint changed or lease lost; not reclaiming",
			"model", rec.ModelID, "instanceId", rec.InstanceID)
		return false, nil
	}
	return err == nil, err
}

// persistObservation CAS-writes the sweep's findings, and only when they
// changed: a steady-state idle endpoint would otherwise cost a KV write per
// tick per endpoint forever.
func (r *Reaper) persistObservation(ctx context.Context, prev, next EndpointRecord) error {
	if prev.LastActiveAt.Equal(next.LastActiveAt) && prev.InFlight == next.InFlight &&
		prev.SuccessTotal == next.SuccessTotal && prev.ScrapeFailures == next.ScrapeFailures {
		return nil
	}
	key := resolveKey(awsidentifiers.GlobalAccountID, next.ModelID)
	current, rev, found, err := r.svc.store.getRevision(ctx, key)
	if err != nil {
		return err
	}
	if !found || current.Generation != prev.Generation || current.State != StateReady || !sameIncarnation(current, prev) {
		// The record moved since this pass read it — a delete, or a relaunch that
		// reused the key. The next sweep re-reads rather than clobbering it.
		return nil
	}
	next.Generation = current.Generation + 1
	if _, err := r.svc.store.CompareAndSet(ctx, key, &next, rev); err != nil {
		if errors.Is(err, kvstore.ErrConflict) {
			return nil
		}
		return err
	}
	return nil
}
