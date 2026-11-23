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

// sweepOnce holds and probes every READY endpoint, acts on what it saw, then re-probes
// parked launches in every account, while the acquisition lost belongs to
// holds. One endpoint's failure does not stop the pass.
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
	if leading(lost) {
		if err := r.recoverParked(ctx, lost); err != nil {
			failures = append(failures, fmt.Errorf("parked recovery: %w", err))
		}
	}
	return errors.Join(failures...)
}

// reaperScrapesMetrics reports whether rec exposes the vLLM Prometheus series
// idle-reclaim reads. Only a bundle with a generative (vLLM) member does; a
// service-only bundle (embed/rerank) has no such load metrics and is
// liveness-probed instead. A record with no members recorded takes the vLLM
// path, where its unknown primary identity keeps it held.
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

// sweepEndpoint holds one READY endpoint, probes it once without following a
// redirect, and releases the hold only on a direct healthy answer. A failed
// probe leaves the hold and changes nothing else: no activity, no teardown.
func (r *Reaper) sweepEndpoint(ctx context.Context, listed EndpointRecord, lost <-chan struct{}) error {
	key := resolveKey(awsidentifiers.GlobalAccountID, listed.ModelID)
	rec, rev, ok, err := r.holdForProbe(ctx, key, lost)
	if err != nil || !ok {
		return err
	}

	scrapeCtx, cancel := context.WithTimeout(ctx, r.scrapeTimeout())
	defer cancel()

	// A bundle with no vLLM member exposes no load metrics to reclaim on, so
	// it is kept warm and only liveness-probed rather than idle-scraped.
	if !reaperScrapesMetrics(rec) {
		r.sweepLiveness(scrapeCtx, key, rec, rev, lost)
		return nil
	}

	model, err := vllmModelFor(rec)
	if err != nil {
		slog.WarnContext(ctx, "bedrock reaper: metrics identity unknown; endpoint held",
			"model", rec.ModelID, "instanceId", rec.InstanceID, "err", err)
		return nil
	}
	sample, err := probeMetrics(scrapeCtx, r.svc.httpClient(), rec.BaseURL, model)
	if err != nil {
		slog.WarnContext(ctx, "bedrock reaper: metrics probe failed; endpoint held",
			"model", rec.ModelID, "instanceId", rec.InstanceID,
			"redirected", errors.Is(err, errMetricsRedirected), "err", err)
		return nil
	}

	now := time.Now().UTC()
	updated, ok := r.releaseHold(ctx, key, rec, rev, lost, func(next *EndpointRecord) {
		next.ScrapeFailures = 0
		next.InFlight = sample.inFlight()
		next.SuccessTotal = float64(sample.successTotal)
		if !sample.idle(rec.SuccessTotal) {
			next.LastActiveAt = now
		}
	})
	if !ok || !r.shouldReap(updated, now) {
		return nil
	}
	slog.InfoContext(ctx, "bedrock reaper: reclaiming an idle endpoint",
		"model", rec.ModelID, "instanceId", rec.InstanceID,
		"idle_ms", otelsetup.Millis(now.Sub(updated.LastActive())))
	if _, err := r.reclaim(ctx, updated, lost); err != nil {
		return fmt.Errorf("reap idle endpoint: %w", err)
	}
	return nil
}

// sweepLiveness keeps a service-only bundle (embed/rerank, no generative
// member) warm without idle-reclaim: a direct 200 on /health refreshes the
// warm clock and releases the hold; anything else leaves the endpoint held.
func (r *Reaper) sweepLiveness(ctx context.Context, key string, rec EndpointRecord, rev uint64, lost <-chan struct{}) {
	if outcome := probeLiveness(ctx, r.svc.httpClient(), rec.BaseURL+"/health"); outcome != livenessLive {
		slog.WarnContext(ctx, "bedrock reaper: liveness probe failed; endpoint held",
			"model", rec.ModelID, "instanceId", rec.InstanceID,
			"endpoint", endpointLogID(rec.BaseURL), "redirected", outcome == livenessRedirected)
		return
	}
	now := time.Now().UTC()
	r.releaseHold(ctx, key, rec, rev, lost, func(next *EndpointRecord) {
		next.ScrapeFailures = 0
		next.LastActiveAt = now
	})
}

// holdForProbe reads key and returns the record and revision of that one read,
// held: an existing hold is observed, otherwise one is reserved by CAS from the
// read. false means yield, with nothing torn down.
func (r *Reaper) holdForProbe(ctx context.Context, key string, lost <-chan struct{}) (EndpointRecord, uint64, bool, error) {
	rec, rev, found, err := r.svc.store.getRevision(ctx, key)
	if err != nil {
		return EndpointRecord{}, 0, false, err
	}
	if !found || rec.State != StateReady {
		return EndpointRecord{}, 0, false, nil
	}
	if resolveAccountID(rec.AccountID) != awsidentifiers.GlobalAccountID ||
		resolveKey(resolveAccountID(rec.AccountID), rec.ModelID) != key {
		slog.WarnContext(ctx, "bedrock reaper: endpoint record does not belong to its key; not probing",
			"model", rec.ModelID)
		return EndpointRecord{}, 0, false, nil
	}
	if rec.Held() {
		return rec, rev, true, nil
	}
	if !leading(lost) {
		return EndpointRecord{}, 0, false, nil
	}
	held, heldRev, err := r.svc.reserveHealthHold(ctx, key, rec, rev, HealthHold{ReservedAt: time.Now().UTC()})
	if err != nil {
		if errors.Is(err, kvstore.ErrConflict) {
			slog.InfoContext(ctx, "bedrock reaper: endpoint changed before its health hold; yielding",
				"model", rec.ModelID, "instanceId", rec.InstanceID)
		} else {
			slog.WarnContext(ctx, "bedrock reaper: reserving a health hold is unconfirmed; yielding",
				"model", rec.ModelID, "instanceId", rec.InstanceID, "err", err)
		}
		return EndpointRecord{}, 0, false, nil
	}
	return held, heldRev, true, nil
}

// releaseHold clears the hold on rec, read at rev, with observe applied in the
// same CAS, after a direct healthy probe. false means the record moved, the
// lease is gone, or the clear's outcome is unconfirmed; nothing is reclaimed.
func (r *Reaper) releaseHold(ctx context.Context, key string, rec EndpointRecord, rev uint64, lost <-chan struct{}, observe func(*EndpointRecord)) (EndpointRecord, bool) {
	if !leading(lost) {
		return EndpointRecord{}, false
	}
	cleared, _, err := r.svc.clearHealthHold(ctx, key, rec, rev, observe)
	if err != nil {
		if errors.Is(err, kvstore.ErrConflict) {
			slog.InfoContext(ctx, "bedrock reaper: held endpoint changed during its probe; yielding",
				"model", rec.ModelID, "instanceId", rec.InstanceID)
		} else {
			slog.WarnContext(ctx, "bedrock reaper: clearing a health hold is unconfirmed; yielding",
				"model", rec.ModelID, "instanceId", rec.InstanceID, "err", err)
		}
		return EndpointRecord{}, false
	}
	return cleared, true
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
