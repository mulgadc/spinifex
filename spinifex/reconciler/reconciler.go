// Package reconciler runs a reconcile function on change rather than on a
// timer: it watches the KV buckets the function reads, coalesces a burst of
// updates into one pass, and falls back to a periodic resync so a gap in the
// watch cannot leave the world unconverged.
package reconciler

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/nats-io/nats.go/jetstream"
)

// Defaults for Config. The resync is the correctness backstop, so it is slow;
// the debounce only has to outlast a multi-key write, so it is short.
const (
	DefaultResync   = 5 * time.Minute
	DefaultDebounce = 250 * time.Millisecond
	// maxDebounce bounds how long a continuous stream of updates can defer a
	// pass, so a busy bucket still reconciles rather than starving.
	maxDebounce = 2 * time.Second
	// retryWatch is how long to wait before re-opening a watcher that could not
	// be established, so a bucket that is briefly unreachable is retried
	// without spinning.
	retryWatch = 5 * time.Second
)

// Source names the buckets to watch and the keys within them that matter.
type Source interface {
	// Buckets is re-evaluated on every resync, so a bucket that appears after
	// startup — a new account's, say — is picked up without a restart.
	Buckets(ctx context.Context) ([]*kvstore.Bucket, error)
	// Filter is a NATS subject filter over key names, e.g. "node.*".
	Filter() string
}

// Config describes one reconcile loop.
type Config struct {
	// Name identifies the loop in logs.
	Name string
	// Sources are watched for changes. An empty Sources is allowed: the loop
	// then runs on the resync alone, which is the pre-watch behaviour.
	Sources []Source
	// Reconcile performs one pass. It is never called concurrently with itself.
	Reconcile func(ctx context.Context) error
	// Resync bounds how stale the world can get when no update arrives, and is
	// also when Sources are re-enumerated. Zero means DefaultResync.
	Resync time.Duration
	// Debounce is how long to wait for a burst to settle. Zero means
	// DefaultDebounce.
	Debounce time.Duration
}

// fixDefaults fills in the zero-valued durations.
func (c *Config) fixDefaults() {
	if c.Resync <= 0 {
		c.Resync = DefaultResync
	}
	if c.Debounce <= 0 {
		c.Debounce = DefaultDebounce
	}
}

// staticSource is one fixed bucket, for the callers whose bucket set does not
// change while the process runs.
type staticSource struct {
	bucket *kvstore.Bucket
	filter string
}

func (s staticSource) Buckets(context.Context) ([]*kvstore.Bucket, error) {
	return []*kvstore.Bucket{s.bucket}, nil
}

func (s staticSource) Filter() string { return s.filter }

// Fixed returns a Source over one bucket whose identity is known at startup.
func Fixed(bucket *kvstore.Bucket, filter string) Source {
	return staticSource{bucket: bucket, filter: filter}
}

var _ Source = staticSource{}

// dynamicSource enumerates its buckets afresh on every resync, for the resource
// families that keep one bucket per account.
type dynamicSource struct {
	list   func(ctx context.Context) ([]*kvstore.Bucket, error)
	filter string
}

func (s dynamicSource) Buckets(ctx context.Context) ([]*kvstore.Bucket, error) {
	return s.list(ctx)
}

func (s dynamicSource) Filter() string { return s.filter }

// Dynamic returns a Source whose bucket set is discovered rather than fixed.
// JetStream has no bucket-created event, so discovery rides on the resync: a
// bucket that appears is watched from the following cycle, and the pass that
// cycle runs covers whatever it already held.
func Dynamic(list func(ctx context.Context) ([]*kvstore.Bucket, error), filter string) Source {
	return dynamicSource{list: list, filter: filter}
}

var _ Source = dynamicSource{}

// Run reconciles once, then on every change and every resync until ctx is done.
// It returns only when ctx is cancelled.
func Run(ctx context.Context, cfg Config) {
	cfg.fixDefaults()
	if cfg.Reconcile == nil {
		return
	}

	// Buffered by one: a change arriving mid-pass sets the pending flag rather
	// than blocking the watcher goroutine, and is served by the next pass.
	changes := make(chan struct{}, 1)
	w := &watchSet{cfg: cfg, changes: changes}
	defer w.stop()

	// Watches go up before the first pass, not after: a change landing between
	// the two would otherwise be invisible until the next resync, because the
	// pass that would have seen it ran before the watch existed.
	w.resync(ctx)
	pass(ctx, cfg, "startup")

	resync := time.NewTicker(cfg.Resync)
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-resync.C:
			// Re-enumerating before the pass means a bucket that appeared since
			// the last resync is watched from here on, and the pass that
			// follows covers whatever it already held.
			w.resync(ctx)
			pass(ctx, cfg, "resync")
		case <-changes:
			if settle(ctx, changes, cfg.Debounce) {
				pass(ctx, cfg, "change")
			}
		}
	}
}

// settle waits for a burst to stop arriving, so one multi-key write produces
// one pass rather than one per key. It gives up waiting after maxDebounce so a
// continuous stream of updates cannot defer the pass indefinitely. It reports
// false only when ctx ended first.
func settle(ctx context.Context, changes <-chan struct{}, debounce time.Duration) bool {
	quiet := time.NewTimer(debounce)
	defer quiet.Stop()
	limit := time.NewTimer(maxDebounce)
	defer limit.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-limit.C:
			return true
		case <-quiet.C:
			return true
		case <-changes:
			if !quiet.Stop() {
				<-quiet.C
			}
			quiet.Reset(debounce)
		}
	}
}

// pass runs one reconcile, logging rather than returning a failure: the loop
// outlives any single pass, and the next resync retries.
func pass(ctx context.Context, cfg Config, trigger string) {
	start := time.Now()
	if err := cfg.Reconcile(ctx); err != nil {
		slog.WarnContext(ctx, "reconcile pass failed, retrying next cycle",
			"loop", cfg.Name, "trigger", trigger, "duration_ms", otelsetup.Millis(time.Since(start)), "err", err)
		return
	}
	slog.DebugContext(ctx, "reconcile pass complete",
		"loop", cfg.Name, "trigger", trigger, "duration_ms", otelsetup.Millis(time.Since(start)))
}

// watchSet holds one watcher per bucket currently being watched, keyed by
// bucket name so a resync can tell an already-watched bucket from a new one.
type watchSet struct {
	cfg     Config
	changes chan struct{}
	active  map[string]*watcher
}

// watcher is one bucket's watcher plus the goroutine draining it.
type watcher struct {
	kw     jetstream.KeyWatcher
	cancel context.CancelFunc
}

// resync opens a watcher for every bucket a Source now names and drops those it
// no longer does. A bucket that cannot be watched is left out and retried on
// the next resync, so one unreachable bucket does not stop the others.
func (w *watchSet) resync(ctx context.Context) {
	if w.active == nil {
		w.active = map[string]*watcher{}
	}
	wanted := map[string]struct{}{}
	for _, src := range w.cfg.Sources {
		buckets, err := src.Buckets(ctx)
		if err != nil {
			// Leave the existing watchers in place: a failed enumeration is not
			// evidence that the buckets went away.
			slog.WarnContext(ctx, "reconcile: enumerate watch buckets failed",
				"loop", w.cfg.Name, "err", err)
			for name := range w.active {
				wanted[name] = struct{}{}
			}
			continue
		}
		for _, bucket := range buckets {
			name := bucket.Name()
			wanted[name] = struct{}{}
			if _, ok := w.active[name]; ok {
				continue
			}
			if watch := w.open(ctx, bucket, src.Filter()); watch != nil {
				w.active[name] = watch
			}
		}
	}
	for name, watch := range w.active {
		if _, ok := wanted[name]; !ok {
			watch.stop()
			delete(w.active, name)
		}
	}
}

// open establishes one watcher and starts draining it. A nil return means the
// bucket could not be watched this cycle.
func (w *watchSet) open(ctx context.Context, bucket *kvstore.Bucket, filter string) *watcher {
	watchCtx, cancel := context.WithCancel(ctx)
	kw, err := bucket.Watch(watchCtx, filter)
	if err != nil {
		cancel()
		slog.WarnContext(ctx, "reconcile: watch unavailable, falling back to resync until it recovers",
			"loop", w.cfg.Name, "bucket", bucket.Name(), "filter", filter, "err", err)
		return nil
	}
	watch := &watcher{kw: kw, cancel: cancel}
	go w.drain(watchCtx, bucket, filter, watch)
	return watch
}

// drain forwards updates until the watcher's channel closes, then re-opens it.
// A closed channel means the connection dropped, so re-establishment is itself
// a change signal: UpdatesOnly hides whatever happened during the gap.
func (w *watchSet) drain(ctx context.Context, bucket *kvstore.Bucket, filter string, watch *watcher) {
	for {
		for update := range watch.kw.Updates() {
			// A nil update marks the end of the initial replay, not a change.
			if update == nil {
				continue
			}
			w.signal()
		}
		if ctx.Err() != nil {
			return
		}
		slog.InfoContext(ctx, "reconcile: watch dropped, re-establishing",
			"loop", w.cfg.Name, "bucket", bucket.Name())
		kw, err := bucket.Watch(ctx, filter)
		if err != nil {
			slog.WarnContext(ctx, "reconcile: re-establish watch failed",
				"loop", w.cfg.Name, "bucket", bucket.Name(), "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryWatch):
			}
			continue
		}
		watch.kw = kw
		w.signal()
	}
}

// signal reports a change without blocking: the channel's single slot already
// means "at least one change is pending", which is all a full-recompute
// reconcile needs to know.
func (w *watchSet) signal() {
	select {
	case w.changes <- struct{}{}:
	default:
	}
}

// stop tears down every watcher.
func (w *watchSet) stop() {
	for name, watch := range w.active {
		watch.stop()
		delete(w.active, name)
	}
}

// stop cancels the draining goroutine and closes the underlying watcher.
// Cancelling already tears the subscription down, so Stop routinely reports an
// invalid subscription on the way out; that is the expected shutdown path.
func (w *watcher) stop() {
	w.cancel()
	if err := w.kw.Stop(); err != nil {
		slog.Debug("reconcile: stopping watcher", "err", err)
	}
}
