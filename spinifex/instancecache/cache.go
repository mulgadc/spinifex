// Package instancecache is a read-only informer over the live instance record
// space: a cache of *vm.VM projections kept current by a JetStream KV watch
// and backstopped by a periodic whole-set resync. It answers List and Get
// from memory; nothing here fans out over NATS.
//
// Entries are immutable. An update replaces a map entry's pointer; nothing
// mutates a *vm.VM in place, and a caller must not either.
//
// The watch and a snapshot are fenced against each other by sequencing, not
// buffering: every sync — initial, periodic, or a dead watcher's replacement
// — takes a Snapshot first, then opens a new watcher that resumes from just
// past the snapshot's high-water mark and replays onto a private candidate
// map until it catches up. Only once that candidate is complete does it
// become the live map, at the same moment the new watcher becomes the active
// one; the old watcher, if there was one, is stopped and joined only after.
// Because the candidate is a map nobody else can see until that swap, a
// failed sync costs nothing: the live map and the previously active watcher
// were never touched.
//
// A record that will not decode never removes an entry: the cache only ever
// removes a key because the record space said it is gone, never because one
// read of it failed.
package instancecache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// Defaults for Config.
const (
	// DefaultResyncInterval is the periodic resync cadence: the completeness
	// backstop for a delete or TTL expiry the watch missed.
	DefaultResyncInterval = 30 * time.Second

	// DefaultRetryInterval is how long a failed sync waits before trying
	// again, so an unreachable bucket is retried without spinning.
	DefaultRetryInterval = 5 * time.Second
)

// Config configures the KV bucket a Cache watches and its clocks. Bucket and
// Prefix are supplied by the caller so this package never has to know the
// daemon's bucket name or key-space constants.
type Config struct {
	// Bucket names and sizes the KV bucket to open. The caller resolves the
	// name and replica count, matching how every other reconcile loop in
	// awsgw opens the same bucket.
	Bucket kvstore.Config

	// Prefix is the key prefix identifying an instance record, e.g. "i.".
	// The watch and snapshot filter is Prefix+"*", and an entry's map key is
	// its record key with Prefix trimmed.
	Prefix string

	ResyncInterval time.Duration
	RetryInterval  time.Duration
}

// liveWatcher is one KeyWatcher plus the context that owns it and the
// goroutine draining it once it is live.
type liveWatcher struct {
	kw     jetstream.KeyWatcher
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// Cache is the informer: a live map of instance ID to *vm.VM, an account
// index over it, and the goroutine that keeps both current. Construct with
// New and start with Run.
type Cache struct {
	store  *kvstore.Store[vm.InstanceRecord]
	prefix string
	filter string

	resyncInterval time.Duration
	retryInterval  time.Duration

	mu      sync.RWMutex
	entries map[string]*vm.VM
	index   map[string]map[string]struct{}
	ready   bool

	watcherMu sync.Mutex
	active    *liveWatcher

	watcherLost chan struct{}
	lastResync  atomic.Int64 // unix nanos of the last successful sync, 0 = never
	degraded    atomic.Bool  // true from a failed sync until the next one succeeds

	metrics *cacheMetrics

	// postSnapshotHook, when set, runs inside syncOnce after snapshotCandidate
	// returns and before the resuming watcher is opened. Nil in production; a
	// test seam for landing a write deterministically inside the window a
	// resuming watcher has to close, which nothing else can reach because it
	// is internal to syncOnce.
	postSnapshotHook func()
}

// New returns a Cache over the bucket cfg describes. Call Run to start it;
// until the first sync completes, List reports the cache not ready.
func New(js jetstream.JetStream, cfg Config) *Cache {
	resync := cfg.ResyncInterval
	if resync <= 0 {
		resync = DefaultResyncInterval
	}
	retry := cfg.RetryInterval
	if retry <= 0 {
		retry = DefaultRetryInterval
	}
	c := &Cache{
		store:          kvstore.New[vm.InstanceRecord](js, cfg.Bucket),
		prefix:         cfg.Prefix,
		filter:         cfg.Prefix + "*",
		resyncInterval: resync,
		retryInterval:  retry,
		entries:        map[string]*vm.VM{},
		index:          map[string]map[string]struct{}{},
		watcherLost:    make(chan struct{}, 1),
	}
	c.metrics = newCacheMetrics(c)
	return c
}

// Run drives the informer until ctx is cancelled: the initial sync, then the
// periodic resync and watcher-replacement loop. Intended to run in its own
// goroutine, matching the other reconcile loops awsgw starts.
func (c *Cache) Run(ctx context.Context) {
	lw := c.initialSync(ctx)
	if lw == nil {
		return
	}

	ticker := time.NewTicker(c.resyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			active := c.getActive()
			if active != nil {
				active.cancel()
				_ = active.kw.Stop()
			}
			return
		case <-c.watcherLost:
			active := c.getActive()
			if active != nil {
				c.replaceWatcher(ctx, active)
			}
		case <-ticker.C:
			c.periodicResync(ctx)
		}
	}
}

// Ready reports whether the initial whole-set sync has completed. Before it
// has, List and Get answer from an empty or partial view and must never be
// read as proof an instance does not exist.
func (c *Cache) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// Degraded reports whether the most recent sync attempt (initial, periodic,
// or watcher-replacement) failed. The cache keeps serving its last-known
// contents regardless, but a cache that has been degraded for a while cannot
// be trusted about absence. Not consulted by List or Get in this phase.
func (c *Cache) Degraded() bool {
	return c.degraded.Load()
}

// List returns the cached instances visible to accountID and whether the
// cache is ready to be believed about absence. IsInstanceVisibleToCaller is
// applied after the index lookup as defence in depth: the index is a
// performance structure, not the authorisation boundary.
func (c *Cache) List(_ context.Context, accountID string) ([]*vm.VM, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := c.index[indexKeyFor(accountID)]
	out := make([]*vm.VM, 0, len(ids))
	for id := range ids {
		v, ok := c.entries[id]
		if !ok || !instance.IsInstanceVisibleToCaller(accountID, v) {
			continue
		}
		out = append(out, v)
	}
	return out, c.ready
}

// Get returns one instance by ID, consulting KV directly on a cache miss. A
// nil instance with a nil error means the record space has no record of it.
func (c *Cache) Get(ctx context.Context, instanceID string) (*vm.VM, error) {
	c.mu.RLock()
	v, ok := c.entries[instanceID]
	c.mu.RUnlock()
	if ok {
		return v, nil
	}

	rec, _, err := c.store.Get(ctx, c.prefix+instanceID)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return vm.VMFromRecord(rec), nil
}

// getActive and setActive guard the watcher pointer separately from the map,
// so a List or Get never blocks behind a watcher transition.
func (c *Cache) getActive() *liveWatcher {
	c.watcherMu.Lock()
	defer c.watcherMu.Unlock()
	return c.active
}

func (c *Cache) setActive(lw *liveWatcher) {
	c.watcherMu.Lock()
	c.active = lw
	c.watcherMu.Unlock()
}

func (c *Cache) markResynced() {
	c.lastResync.Store(time.Now().UnixNano())
	c.degraded.Store(false)
}

// resyncAge reports how long ago the last successful sync completed. ok is
// false when no sync has ever succeeded, which the resync-age metric skips
// rather than reporting a misleading zero.
func (c *Cache) resyncAge() (time.Duration, bool) {
	n := c.lastResync.Load()
	if n == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, n)), true
}

// entriesByState groups the live map by raw instance state, for the
// entries-by-state gauge.
func (c *Cache) entriesByState() map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]int64, len(c.entries))
	for _, v := range c.entries {
		out[string(v.Status)]++
	}
	return out
}

// sleep waits out the retry interval, reporting false if ctx ended first.
func (c *Cache) sleep(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(c.retryInterval):
		return true
	}
}

// syncFailed records a failed sync attempt: the metric, the degraded flag,
// and a warning log, shared by every sync path so a failure looks the same
// regardless of which one hit it.
func (c *Cache) syncFailed(ctx context.Context, err error) {
	c.metrics.resyncFailed()
	c.degraded.Store(true)
	slog.WarnContext(ctx, "instancecache: sync failed", "err", err)
}

// drain applies every event lw's watcher delivers, live, once the sync that
// installed lw has already caught it up: a channel that closes while lw's
// context is still live means the connection dropped out from under it,
// which is the signal Run replaces the watcher on; a channel that closes
// because the context was cancelled is an intentional teardown and reports
// nothing. An event delivered after lw has been superseded as the active
// watcher is dropped rather than applied: the watcher that replaced it is
// independently live-subscribed and already covers it, so nothing is lost,
// and this keeps it true that only the active watcher ever writes to the
// live map.
func (c *Cache) drain(lw *liveWatcher) {
	defer close(lw.done)
	for {
		select {
		case <-lw.ctx.Done():
			return
		case entry, ok := <-lw.kw.Updates():
			if !ok {
				if lw.ctx.Err() == nil && c.getActive() == lw {
					select {
					case c.watcherLost <- struct{}{}:
					default:
					}
				}
				return
			}
			if entry == nil {
				continue
			}
			if c.getActive() != lw {
				continue
			}
			c.applyLive(entry)
		}
	}
}

func (c *Cache) applyLive(entry jetstream.KeyValueEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apply(c.entries, c.index, entry)
}

// apply decodes one watch entry into entries/index. A put that will not
// decode leaves whatever is already at that key untouched and counts a
// decode failure; a delete or purge always removes the key. Absence is
// established only by the record space saying so, never by a decode failure.
func (c *Cache) apply(entries map[string]*vm.VM, index map[string]map[string]struct{}, entry jetstream.KeyValueEntry) {
	id := strings.TrimPrefix(entry.Key(), c.prefix)
	switch entry.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		removeFrom(entries, index, id)
	default:
		var rec vm.InstanceRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			c.metrics.decodeFailure()
			slog.Warn("instancecache: undecodable record, keeping previous value",
				"key", entry.Key(), "err", err)
			return
		}
		putInto(entries, index, id, vm.VMFromRecord(&rec))
	}
}

// snapshotCandidate reads the whole record space through one consumer,
// mutually consistent per kvstore.Store.Snapshot, and builds a fresh
// entries/index pair from it.
func (c *Cache) snapshotCandidate(ctx context.Context) (map[string]*vm.VM, map[string]map[string]struct{}, uint64, error) {
	items, highWater, err := c.store.Snapshot(ctx, c.filter)
	if err != nil {
		return nil, nil, 0, err
	}
	entries := make(map[string]*vm.VM, len(items))
	index := make(map[string]map[string]struct{})
	for i := range items {
		id := strings.TrimPrefix(items[i].Key, c.prefix)
		putInto(entries, index, id, vm.VMFromRecord(&items[i].Value))
	}
	return entries, index, highWater, nil
}

// replay applies every event kw delivers onto entries/index until the
// end-of-replay marker (nil) arrives: the candidate then reflects everything
// between the snapshot's high-water mark and the moment it caught up, which
// is the whole fence — no buffer, no mode, just sequencing.
func (c *Cache) replay(ctx context.Context, kw jetstream.KeyWatcher, entries map[string]*vm.VM, index map[string]map[string]struct{}) (int, error) {
	applied := 0
	for {
		select {
		case <-ctx.Done():
			return applied, ctx.Err()
		case entry, ok := <-kw.Updates():
			if !ok {
				return applied, errors.New("instancecache: watcher closed before catching up")
			}
			if entry == nil {
				return applied, nil
			}
			c.apply(entries, index, entry)
			applied++
		}
	}
}

// syncOnce runs the whole build algorithm once: snapshot the record space,
// open a watcher that resumes from just past the snapshot's high-water mark,
// and replay onto the candidate until it catches up. It touches neither the
// live map nor the active watcher — installSync does that, and only once an
// attempt fully succeeds, so a failed attempt has cost nothing.
func (c *Cache) syncOnce(ctx context.Context) (*liveWatcher, map[string]*vm.VM, map[string]map[string]struct{}, error) {
	entries, index, hw, err := c.snapshotCandidate(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if c.postSnapshotHook != nil {
		c.postSnapshotHook()
	}

	watchCtx, cancel := context.WithCancel(ctx)
	kw, err := c.store.WatchFrom(watchCtx, c.filter, hw+1)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}

	replayed, err := c.replay(watchCtx, kw, entries, index)
	if err != nil {
		cancel()
		_ = kw.Stop()
		return nil, nil, nil, err
	}
	c.metrics.replayApplied(replayed)
	slog.Debug("instancecache: sync caught up", "snapshot_entries", len(entries),
		"high_water", hw, "replayed", replayed)

	return &liveWatcher{kw: kw, ctx: watchCtx, cancel: cancel, done: make(chan struct{})}, entries, index, nil
}

// installSync swaps entries/index in as the live map and lw in as the active
// watcher together, starts lw's live-apply goroutine, then stops and joins
// oldLw. In that order, so there is never a moment where two watchers both
// apply to the live map: oldLw, while it was still active, only ever applied
// to the map installSync is now replacing, and it stops being active in the
// same step that replacement happens.
func (c *Cache) installSync(lw *liveWatcher, entries map[string]*vm.VM, index map[string]map[string]struct{}, oldLw *liveWatcher) {
	c.mu.Lock()
	c.entries, c.index = entries, index
	c.ready = true
	c.mu.Unlock()
	c.setActive(lw)
	go c.drain(lw)

	if oldLw != nil {
		oldLw.cancel()
		_ = oldLw.kw.Stop()
		<-oldLw.done
	}
}

// resyncUntilSuccess retries syncOnce against the retry interval until it
// succeeds or ctx ends, installing the result over oldLw. Used by the
// initial sync and watcher replacement, which both must block until a live
// watcher exists again; a periodic resync tries only once per tick instead.
func (c *Cache) resyncUntilSuccess(ctx context.Context, oldLw *liveWatcher) *liveWatcher {
	for {
		lw, entries, index, err := c.syncOnce(ctx)
		if err != nil {
			c.syncFailed(ctx, err)
			if !c.sleep(ctx) {
				return nil
			}
			continue
		}
		c.installSync(lw, entries, index, oldLw)
		c.markResynced()
		return lw
	}
}

// initialSync is resyncUntilSuccess with no old watcher to fence against,
// plus making the result the active watcher and recording the first
// successful resync.
func (c *Cache) initialSync(ctx context.Context) *liveWatcher {
	return c.resyncUntilSuccess(ctx, nil)
}

// periodicResync tries the build algorithm once per tick. The active watcher
// keeps applying to the live map for the whole attempt, since installSync is
// the only thing that ever changes what "the live map" is, so a failure
// costs nothing but a discarded candidate; the next tick tries again rather
// than retrying in a loop here.
func (c *Cache) periodicResync(ctx context.Context) {
	oldLw := c.getActive()
	if oldLw == nil {
		return
	}
	lw, entries, index, err := c.syncOnce(ctx)
	if err != nil {
		c.syncFailed(ctx, err)
		return
	}
	c.installSync(lw, entries, index, oldLw)
	c.markResynced()
}

// replaceWatcher retires a dead watcher and installs a fresh one under the
// same build algorithm, retrying against the retry interval until it
// succeeds or ctx ends.
func (c *Cache) replaceWatcher(ctx context.Context, oldLw *liveWatcher) {
	lw := c.resyncUntilSuccess(ctx, oldLw)
	if lw != nil {
		c.metrics.watchReconnected()
	}
}

// indexKeyFor is the account index's bucket for v's owner: Global for an
// empty owner, so a legacy pre-account record files under the same bucket a
// platform-managed record does, and never under every account at once.
func indexKeyFor(accountID string) string {
	if accountID == "" {
		return utils.GlobalAccountID
	}
	return accountID
}

func putInto(entries map[string]*vm.VM, index map[string]map[string]struct{}, id string, v *vm.VM) {
	if old, ok := entries[id]; ok {
		removeFromIndex(index, indexKeyFor(old.AccountID), id)
	}
	entries[id] = v
	addToIndex(index, indexKeyFor(v.AccountID), id)
}

func removeFrom(entries map[string]*vm.VM, index map[string]map[string]struct{}, id string) {
	old, ok := entries[id]
	if !ok {
		return
	}
	delete(entries, id)
	removeFromIndex(index, indexKeyFor(old.AccountID), id)
}

func addToIndex(index map[string]map[string]struct{}, key, id string) {
	set, ok := index[key]
	if !ok {
		set = make(map[string]struct{})
		index[key] = set
	}
	set[id] = struct{}{}
}

func removeFromIndex(index map[string]map[string]struct{}, key, id string) {
	set, ok := index[key]
	if !ok {
		return
	}
	delete(set, id)
	if len(set) == 0 {
		delete(index, key)
	}
}
