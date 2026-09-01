// Package reconciler runs a reconcile function on change rather than on a
// timer: it watches the KV buckets the function reads, coalesces a burst of
// updates into one pass, and falls back to a periodic resync so a gap in the
// watch cannot leave the world unconverged.
//
// A caller that can act on one resource at a time also sets ReconcileKey, and
// the key that changed is carried through to it. The whole-set Reconcile stays
// required either way: it is the startup pass and the resync backstop, so a key
// the queue never learns about still converges.
//
// A pass may also name a deadline to be revisited by. That is for the loops
// waiting on something no KV write announces — a VM reaching running, an agent
// falling silent, a timeout expiring — which a watch cannot see at all, and for
// which the resync alone is too blunt to be a failure detector.
//
// A caller whose inputs are not all KV can supply a Trigger channel as well, for
// the events that arrive by some other route and would otherwise be seen only
// when the next pass happened to look.
package reconciler

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
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
	// DefaultErrorRetry is how soon a pass that failed transiently asks to be
	// revisited. Waiting out the resync leaves the world unconverged for minutes
	// over a fault that usually clears in seconds.
	DefaultErrorRetry = 15 * time.Second
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
	// Reconcile performs one whole-set pass. It is never called concurrently
	// with itself or with ReconcileKey.
	//
	// Required even when ReconcileKey is set, because it is the startup pass
	// and the resync backstop. A per-key queue only converges if something
	// behind it covers what the queue never learned: a key deleted while the
	// watch was down is enqueued by nothing, since nobody observes an absence.
	//
	// The duration it returns is a revisit deadline: run me again within this,
	// even if nothing changes. Zero means no deadline, which is the right answer
	// for a loop that is purely a function of the buckets it watches. It is for
	// the loops that are not — the ones waiting on a VM to reach running, an
	// endpoint to answer, or an agent to fall silent, none of which write to KV
	// and so none of which a watch can see.
	Reconcile func(ctx context.Context) (revisit time.Duration, err error)
	// ReconcileKey handles one changed key, for a caller whose work is per
	// resource rather than a recompute of the whole set. Unset means every
	// change runs Reconcile, which is the whole-set behaviour.
	//
	// It is called once per distinct key in a burst, so a caller whose unit of
	// work is coarser than a key — an account rather than an instance — maps
	// updates onto that unit with KeyFor rather than deduplicating here.
	//
	// Its revisit deadline means the same thing as Reconcile's, and is honoured
	// by running a whole-set pass: one key's view cannot say what the others
	// need, and only a whole-set pass can re-derive the deadline for all of them.
	ReconcileKey func(ctx context.Context, key string) (revisit time.Duration, err error)
	// KeyFor maps a watch update onto the keys it dirties. Nil means the key
	// that changed is the key to reconcile.
	//
	// Reporting ok=false means the update could not be attributed, and the
	// change runs Reconcile instead. That is the honest answer for a delete: a
	// tombstone carries no value, so a caller whose work key lives in the value
	// cannot name it, and a whole-set pass needs no attribution.
	KeyFor func(entry jetstream.KeyValueEntry) (keys []string, ok bool)
	// Trigger wakes the loop from outside the buckets, for the inputs a KV watch
	// cannot see because they never reach KV. A receive is treated exactly like a
	// watch update: debounced with them, then served by a whole-set pass, since an
	// external signal names no key. Nil means the buckets are the only source.
	Trigger <-chan struct{}
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
		// Setting only ReconcileKey looks like a working loop and is not one:
		// nothing would run the startup pass or the resync backstop.
		slog.ErrorContext(ctx, "reconcile loop not started: Reconcile is required", "loop", cfg.Name)
		return
	}

	// Buffered by one: a change arriving mid-pass sets the pending flag rather
	// than blocking the watcher goroutine, and is served by the next pass. The
	// keys themselves ride in the queue; this only says "something is waiting".
	changes := make(chan struct{}, 1)
	queue := &workQueue{}
	w := &watchSet{cfg: cfg, changes: changes, queue: queue}
	defer w.stop()

	// Watches go up before the first pass, not after: a change landing between
	// the two would otherwise be invisible until the next resync, because the
	// pass that would have seen it ran before the watch existed.
	w.resync(ctx)

	if cfg.Trigger != nil {
		go forward(ctx, cfg.Trigger, w)
	}

	resync := time.NewTicker(cfg.Resync)
	defer resync.Stop()

	// Fires when a pass asked to be revisited sooner than the resync. Created
	// already expired and drained, so a loop whose passes never ask keeps the
	// resync as its only timer.
	revisit := time.NewTimer(0)
	<-revisit.C
	defer revisit.Stop()

	rearm(revisit, clampRevisit(pass(ctx, cfg, "startup"), cfg.Resync))

	for {
		select {
		case <-ctx.Done():
			return
		case <-resync.C:
			// Re-enumerating before the pass means a bucket that appeared since
			// the last resync is watched from here on, and the pass that
			// follows covers whatever it already held.
			w.resync(ctx)
			// Whatever is queued is covered by the whole-set pass about to run,
			// so it is dropped rather than reconciled again after it.
			queue.take()
			rearm(revisit, clampRevisit(pass(ctx, cfg, "resync"), cfg.Resync))
		case <-revisit.C:
			// A deadline is always served by a whole-set pass: the caller asked
			// to be revisited without anything having changed, so there is no
			// key to name, and only a whole-set pass can set the next deadline.
			queue.take()
			rearm(revisit, clampRevisit(pass(ctx, cfg, "deadline"), cfg.Resync))
		case <-changes:
			if settle(ctx, changes, cfg.Debounce) {
				rearm(revisit, clampRevisit(serve(ctx, cfg, queue), cfg.Resync))
			}
		}
	}
}

// forward turns an external wake into the same signal a watch update produces,
// so a trigger is debounced and coalesced with them rather than taking a second
// path through the loop. It queues the whole set because a trigger names no key.
func forward(ctx context.Context, trigger <-chan struct{}, w *watchSet) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-trigger:
			if !open {
				return
			}
			w.queue.addWhole()
			w.signal()
		}
	}
}

// clampRevisit bounds a deadline to the resync. A caller may ask to be revisited
// sooner but never later: the resync is the outer bound on staleness, and is not
// something a pass gets to extend. Non-positive means no deadline.
func clampRevisit(d, resync time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return min(d, resync)
}

// rearm resets a timer that may or may not have fired, and leaves it stopped for
// a zero duration. Draining is conditional because the loop consumes the tick
// itself on the path that a deadline pass runs from.
func rearm(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	if d > 0 {
		t.Reset(d)
	}
}

// Earliest is the soonest of two revisit deadlines, treating a non-positive one
// as "no deadline" rather than as "immediately". Exported because a whole-set
// pass has to fold one of these per resource before it can return just one.
func Earliest(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

// serve runs the work a settled burst produced: one whole-set pass, or one pass
// per distinct key that changed. It reports the soonest deadline those passes
// asked for.
func serve(ctx context.Context, cfg Config, queue *workQueue) time.Duration {
	if cfg.ReconcileKey == nil {
		return pass(ctx, cfg, "change")
	}
	whole, keys := queue.take()
	if whole {
		// A whole-set pass covers every key that was waiting alongside it, so
		// the keys it returned are deliberately not reconciled again.
		return pass(ctx, cfg, "change")
	}
	var revisit time.Duration
	for _, key := range keys {
		if ctx.Err() != nil {
			return revisit
		}
		revisit = Earliest(revisit, keyPass(ctx, cfg, key))
	}
	return revisit
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

// failed reports a reconcile failure and returns the deadline it should be
// retried on. A terminal error will fail identically however often it is
// repeated, so it is reported as needing intervention and asks for nothing
// sooner than the resync; anything else is retried promptly.
//
// A deadline the callee asked for survives when it is sooner: a pass partway
// through a transition still gets revisited on its own schedule.
func failed(ctx context.Context, loop, what string, err error, revisit time.Duration, attrs ...any) time.Duration {
	attrs = append([]any{"loop", loop}, append(attrs, "err", err)...)
	if awserrors.IsTerminal(err) {
		slog.ErrorContext(ctx, what+" failed and will not succeed unchanged", attrs...)
		return revisit
	}
	slog.WarnContext(ctx, what+" failed, retrying", attrs...)
	return Earliest(revisit, DefaultErrorRetry)
}

// pass runs one reconcile, logging rather than returning a failure: the loop
// outlives any single pass, and the next resync retries. A failed pass keeps
// its deadline, so a caller partway through a transition still gets revisited
// on its own schedule rather than dropping back to the resync.
func pass(ctx context.Context, cfg Config, trigger string) time.Duration {
	start := time.Now()
	revisit, err := cfg.Reconcile(ctx)
	if err != nil {
		return failed(ctx, cfg.Name, "reconcile pass", err, revisit,
			"trigger", trigger, "duration_ms", otelsetup.Millis(time.Since(start)))
	}
	slog.DebugContext(ctx, "reconcile pass complete",
		"loop", cfg.Name, "trigger", trigger, "duration_ms", otelsetup.Millis(time.Since(start)),
		"revisit_ms", otelsetup.Millis(revisit))
	return revisit
}

// keyPass runs one per-key reconcile. The resync re-reconciles the whole set, so
// a key that failed is covered by the same backstop as a key the queue never
// saw; the deadline a transient failure asks for only brings that forward.
func keyPass(ctx context.Context, cfg Config, key string) time.Duration {
	start := time.Now()
	revisit, err := cfg.ReconcileKey(ctx, key)
	if err != nil {
		return failed(ctx, cfg.Name, "reconcile key", err, revisit,
			"key", key, "duration_ms", otelsetup.Millis(time.Since(start)))
	}
	slog.DebugContext(ctx, "reconcile key complete",
		"loop", cfg.Name, "key", key, "duration_ms", otelsetup.Millis(time.Since(start)),
		"revisit_ms", otelsetup.Millis(revisit))
	return revisit
}

// watchSet holds one watcher per bucket currently being watched, keyed by
// bucket name so a resync can tell an already-watched bucket from a new one.
type watchSet struct {
	cfg     Config
	changes chan struct{}
	queue   *workQueue
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
			w.observe(update)
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
		// The whole set, not a key: UpdatesOnly hides what happened during the
		// gap, so what changed is exactly what cannot be named.
		w.queue.addWhole()
		w.signal()
	}
}

// observe queues the work an update implies and wakes the loop.
func (w *watchSet) observe(entry jetstream.KeyValueEntry) {
	switch {
	case w.cfg.ReconcileKey == nil:
		// Whole-set callers never read the key, so nothing is queued for them.
	case w.cfg.KeyFor == nil:
		w.queue.add(entry.Key())
	default:
		keys, ok := w.cfg.KeyFor(entry)
		if !ok {
			w.queue.addWhole()
			break
		}
		w.queue.add(keys...)
	}
	w.signal()
}

// signal reports that work is waiting without blocking: the channel's single
// slot already means "at least one change is pending", and for a per-key loop
// what is pending is in the queue rather than in the channel.
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
