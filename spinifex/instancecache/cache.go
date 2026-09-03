// Package instancecache is a read-only informer over the live instance record
// space: a cache of *vm.VM projections kept current by a JetStream KV watch
// and backstopped by a periodic whole-set resync. It answers List and Get
// from memory; nothing here fans out over NATS.
//
// Entries are immutable. An update replaces a map entry's pointer; nothing
// mutates a *vm.VM in place, and a caller must not either.
//
// The watch and the resync are fenced against each other through
// kvstore.Store.Snapshot's highWater mark: a watcher is opened and its events
// buffered before the snapshot is taken, so nothing that happened during the
// snapshot is lost or double-applied once the buffer is drained against the
// snapshot's high-water revision. A periodic resync does not open a second
// subscription — it tees the existing live watcher's events into a side
// buffer while continuing to apply them to the live map directly, so a
// resync that then fails has cost nothing: the live map already has
// everything the failed candidate would have had. Only a watcher that
// actually dies is replaced, and its successor takes over under the same
// fencing before the old one is stopped and joined.
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
	// DefaultResyncInterval is the periodic tee-based resync cadence: the
	// completeness backstop for a delete or TTL expiry the watch missed.
	DefaultResyncInterval = 30 * time.Second

	// DefaultMaxBufferedEvents bounds the side buffer a sync fences events
	// through. Overflowing it fails the sync rather than dropping events
	// silently, so it must stay well above a burst like a 50-instance launch.
	DefaultMaxBufferedEvents = 10000

	// DefaultRetryInterval is how long a failed sync waits before trying
	// again, so an unreachable bucket is retried without spinning.
	DefaultRetryInterval = 5 * time.Second
)

var errBufferOverflow = errors.New("instancecache: sync buffer overflow")

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

	ResyncInterval    time.Duration
	MaxBufferedEvents int
	RetryInterval     time.Duration
}

// watchMode governs what a liveWatcher's drain goroutine does with an
// incoming event.
type watchMode int

const (
	// modeBuffer queues events without applying them: a watcher that has not
	// yet gone live, either the very first one or a replacement mid-handoff.
	modeBuffer watchMode = iota
	// modeLive applies events directly and buffers nothing.
	modeLive
	// modeTee applies events directly and also queues them, for a periodic
	// resync borrowing the live watcher's events without diverting them.
	modeTee
)

// liveWatcher is one KeyWatcher plus the goroutine draining it, and the
// buffering state a sync uses to fence its events against a snapshot.
type liveWatcher struct {
	kw     jetstream.KeyWatcher
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	mode   watchMode
	buf    []jetstream.KeyValueEntry
	bufErr error
}

// Cache is the informer: a live map of instance ID to *vm.VM, an account
// index over it, and the goroutine that keeps both current. Construct with
// New and start with Run.
type Cache struct {
	store  *kvstore.Store[vm.InstanceRecord]
	prefix string
	filter string

	resyncInterval    time.Duration
	maxBufferedEvents int
	retryInterval     time.Duration

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

	// postSnapshotHook, when set, runs after Snapshot returns and before the
	// buffer is read. Nil in production; a test seam for landing an event
	// deterministically inside the fence's danger window.
	postSnapshotHook func()
}

// New returns a Cache over the bucket cfg describes. Call Run to start it;
// until the first sync completes, List reports the cache not ready.
func New(js jetstream.JetStream, cfg Config) *Cache {
	resync := cfg.ResyncInterval
	if resync <= 0 {
		resync = DefaultResyncInterval
	}
	maxBuf := cfg.MaxBufferedEvents
	if maxBuf <= 0 {
		maxBuf = DefaultMaxBufferedEvents
	}
	retry := cfg.RetryInterval
	if retry <= 0 {
		retry = DefaultRetryInterval
	}
	c := &Cache{
		store:             kvstore.New[vm.InstanceRecord](js, cfg.Bucket),
		prefix:            cfg.Prefix,
		filter:            cfg.Prefix + "*",
		resyncInterval:    resync,
		maxBufferedEvents: maxBuf,
		retryInterval:     retry,
		entries:           map[string]*vm.VM{},
		index:             map[string]map[string]struct{}{},
		watcherLost:       make(chan struct{}, 1),
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

// openWatcher opens a new watcher in modeBuffer and starts draining it. A nil
// return means the bucket could not be watched this attempt.
func (c *Cache) openWatcher(ctx context.Context) *liveWatcher {
	watchCtx, cancel := context.WithCancel(ctx)
	kw, err := c.store.Watch(watchCtx, c.filter)
	if err != nil {
		cancel()
		slog.WarnContext(ctx, "instancecache: watch unavailable, retrying", "err", err)
		return nil
	}
	lw := &liveWatcher{kw: kw, cancel: cancel, done: make(chan struct{})}
	go c.drain(watchCtx, lw)
	return lw
}

// discardWatcher tears a failed candidate's watcher down: cancel, stop and
// join, so a repeatedly failing sync leaks neither a subscription nor a
// buffer.
func (c *Cache) discardWatcher(lw *liveWatcher) {
	lw.cancel()
	_ = lw.kw.Stop()
	<-lw.done
}

// drain forwards one watcher's events to handleEntry until its channel
// closes or ctx ends. A channel that closes while ctx is still live means the
// connection dropped out from under it, which is the signal Run replaces the
// watcher on; a channel that closes because ctx was cancelled is an
// intentional teardown and reports nothing.
func (c *Cache) drain(ctx context.Context, lw *liveWatcher) {
	defer close(lw.done)
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-lw.kw.Updates():
			if !ok {
				if ctx.Err() == nil {
					select {
					case c.watcherLost <- struct{}{}:
					default:
					}
				}
				return
			}
			if entry == nil {
				// UpdatesOnly should not emit the end-of-replay marker, but a
				// nil entry carries no operation to apply either way.
				continue
			}
			c.handleEntry(lw, entry)
		}
	}
}

// handleEntry buffers entry when lw's mode says to, and applies it live when
// lw's mode says to. The two are independent so a tee does both.
func (c *Cache) handleEntry(lw *liveWatcher, entry jetstream.KeyValueEntry) {
	lw.mu.Lock()
	mode := lw.mode
	if mode == modeBuffer || mode == modeTee {
		if lw.bufErr == nil {
			if len(lw.buf) >= c.maxBufferedEvents {
				lw.bufErr = errBufferOverflow
			} else {
				lw.buf = append(lw.buf, entry)
			}
		}
	}
	lw.mu.Unlock()

	if mode == modeLive || mode == modeTee {
		c.applyLive(entry)
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
	if c.postSnapshotHook != nil {
		c.postSnapshotHook()
	}
	entries := make(map[string]*vm.VM, len(items))
	index := make(map[string]map[string]struct{})
	for i := range items {
		id := strings.TrimPrefix(items[i].Key, c.prefix)
		putInto(entries, index, id, vm.VMFromRecord(&items[i].Value))
	}
	return entries, index, highWater, nil
}

// drainBuffered applies every buffered event whose revision is newer than
// highWater onto entries/index, which is the fence: anything at or before
// highWater is already reflected in the snapshot that produced entries.
func (c *Cache) drainBuffered(entries map[string]*vm.VM, index map[string]map[string]struct{},
	buf []jetstream.KeyValueEntry, highWater uint64) {
	for _, entry := range buf {
		if entry.Revision() <= highWater {
			continue
		}
		c.apply(entries, index, entry)
	}
}

// goLive ends a watcher's buffered phase: further events apply directly, and
// anything already queued during the handoff is applied now, live, rather
// than silently kept or dropped.
func (c *Cache) goLive(lw *liveWatcher) {
	lw.mu.Lock()
	leftover := lw.buf
	lw.buf = nil
	lw.bufErr = nil
	lw.mode = modeLive
	lw.mu.Unlock()
	for _, entry := range leftover {
		c.applyLive(entry)
	}
}

// freshSync opens a new watcher, buffers its events, and fences a snapshot
// against them per the same algorithm every sync uses. It retries against
// ctx's retry interval until it succeeds or ctx ends. On success the new
// watcher is live and the map holds the freshly built candidate; on ctx
// ending it returns nil having left nothing behind.
func (c *Cache) freshSync(ctx context.Context) *liveWatcher {
	for {
		lw := c.openWatcher(ctx)
		if lw == nil {
			if !c.sleep(ctx) {
				return nil
			}
			continue
		}

		entries, index, hw, err := c.snapshotCandidate(ctx)
		if err == nil {
			lw.mu.Lock()
			buf, bufErr := lw.buf, lw.bufErr
			lw.mu.Unlock()
			if bufErr == nil {
				c.drainBuffered(entries, index, buf, hw)
				c.mu.Lock()
				c.entries, c.index = entries, index
				c.ready = true
				c.mu.Unlock()
				c.goLive(lw)
				return lw
			}
			err = bufErr
		}

		c.metrics.resyncFailed()
		c.degraded.Store(true)
		slog.WarnContext(ctx, "instancecache: sync failed, retrying", "err", err)
		c.discardWatcher(lw)
		if !c.sleep(ctx) {
			return nil
		}
	}
}

// initialSync is freshSync plus making the result the active watcher and
// recording the first successful resync.
func (c *Cache) initialSync(ctx context.Context) *liveWatcher {
	lw := c.freshSync(ctx)
	if lw == nil {
		return nil
	}
	c.setActive(lw)
	c.markResynced()
	return lw
}

// periodicResync tees the live watcher rather than opening a second
// subscription: events keep applying to the live map throughout, and a
// snapshot-fenced candidate is built off to the side and swapped in only on
// success. A failure costs nothing but a discarded candidate, because the
// tee already applied everything to the live map in real time.
func (c *Cache) periodicResync(ctx context.Context) {
	lw := c.getActive()
	if lw == nil {
		return
	}

	lw.mu.Lock()
	lw.mode = modeTee
	lw.buf = nil
	lw.bufErr = nil
	lw.mu.Unlock()

	entries, index, hw, err := c.snapshotCandidate(ctx)
	if err == nil {
		lw.mu.Lock()
		buf, bufErr := lw.buf, lw.bufErr
		lw.mu.Unlock()
		if bufErr == nil {
			c.drainBuffered(entries, index, buf, hw)
			c.mu.Lock()
			c.entries, c.index = entries, index
			c.mu.Unlock()
			c.markResynced()
		} else {
			err = bufErr
		}
	}
	if err != nil {
		c.metrics.resyncFailed()
		c.degraded.Store(true)
		slog.WarnContext(ctx, "instancecache: periodic resync failed, serving previous contents", "err", err)
	}
	c.goLive(lw)
}

// replaceWatcher retires a dead watcher and installs a fresh one under the
// same fencing. The old watcher is stopped and joined before the new one is
// allowed to go live, so there is never a moment where two watchers both
// apply to the map.
func (c *Cache) replaceWatcher(ctx context.Context, oldLw *liveWatcher) {
	oldLw.cancel()
	_ = oldLw.kw.Stop()
	<-oldLw.done

	lw := c.freshSync(ctx)
	if lw == nil {
		return
	}
	c.setActive(lw)
	c.metrics.watchReconnected()
	c.markResynced()
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
