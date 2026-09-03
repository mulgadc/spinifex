// internal mode, buffer and subscription state directly (there is no
// exported surface for "one live watcher" or "buffer returned to its
// starting size"), and the account-index defence-in-depth test deliberately
// mis-files a record by writing straight into the unexported maps.
//
//test:in-package — the fencing and lifecycle tests assert on the watcher's
package instancecache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	testPrefix = "i."
	acctA      = "111122223333"
	acctB      = "444455556666"
)

// newTestCache starts an embedded JetStream server, opens a KV bucket for it
// and returns a Cache over that bucket plus the raw handles a test needs to
// drive the underlying key space directly.
func newTestCache(t *testing.T, mutate ...func(*Config)) (*Cache, jetstream.KeyValue, *cacheConn) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	bucket := fmt.Sprintf("instances-%d", time.Now().UnixNano())
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)

	cfg := Config{
		Bucket:        kvstore.Config{Name: bucket, History: 1, Replicas: 1},
		Prefix:        testPrefix,
		RetryInterval: 20 * time.Millisecond,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return New(js, cfg), kv, &cacheConn{nc: nc, js: js}
}

// cacheConn bundles the connection handles a test occasionally needs beyond
// the Cache and its bucket, named to avoid importing nats.go just for a type.
type cacheConn struct {
	nc interface{ NumSubscriptions() int }
	js jetstream.JetStream
}

func testRecord(id, accountID string, state vm.InstanceState) *vm.InstanceRecord {
	return &vm.InstanceRecord{
		Metadata: resource.Metadata{Name: id, AccountID: accountID},
		Spec:     vm.InstanceSpec{InstanceType: "m7i.large"},
		Status:   vm.InstanceStatus{Status: state, LastNode: "node-1"},
	}
}

func putRecord(t *testing.T, kv jetstream.KeyValue, id string, rec *vm.InstanceRecord) uint64 {
	t.Helper()
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	rev, err := kv.Put(context.Background(), testPrefix+id, data)
	require.NoError(t, err)
	return rev
}

func putGarbage(t *testing.T, kv jetstream.KeyValue, id string) uint64 {
	t.Helper()
	rev, err := kv.Put(context.Background(), testPrefix+id, []byte("not-json"))
	require.NoError(t, err)
	return rev
}

func idsOf(list []*vm.VM) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = v.ID
	}
	return out
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForReady(t *testing.T, c *Cache) {
	t.Helper()
	waitFor(t, 2*time.Second, c.Ready)
}

func waitForListLen(t *testing.T, c *Cache, accountID string, n int) []*vm.VM {
	t.Helper()
	var list []*vm.VM
	waitFor(t, 2*time.Second, func() bool {
		list, _ = c.List(context.Background(), accountID)
		return len(list) == n
	})
	return list
}

// fakeEntry is a minimal jetstream.KeyValueEntry for exercising the fencing
// logic directly, without a real watcher's timing involved.
type fakeEntry struct {
	key   string
	value []byte
	rev   uint64
	op    jetstream.KeyValueOp
}

func (f fakeEntry) Bucket() string                  { return "test" }
func (f fakeEntry) Key() string                     { return f.key }
func (f fakeEntry) Value() []byte                   { return f.value }
func (f fakeEntry) Revision() uint64                { return f.rev }
func (f fakeEntry) Created() time.Time              { return time.Time{} }
func (f fakeEntry) Delta() uint64                   { return 0 }
func (f fakeEntry) Operation() jetstream.KeyValueOp { return f.op }

var _ jetstream.KeyValueEntry = fakeEntry{}

func putEntry(t *testing.T, id string, rec *vm.InstanceRecord, rev uint64) fakeEntry {
	t.Helper()
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	return fakeEntry{key: testPrefix + id, value: data, rev: rev, op: jetstream.KeyValuePut}
}

func deleteEntry(id string, rev uint64) fakeEntry {
	return fakeEntry{key: testPrefix + id, rev: rev, op: jetstream.KeyValueDelete}
}

// Process-wide manual metric reader, installed once per test binary so the
// resync-failure and decode-failure counters can be observed. Cumulative for
// the life of the binary, so tests read a before/after delta.
var (
	metricsReaderOnce sync.Once
	metricsReader     *sdkmetric.ManualReader
)

func installMetricsReader(t *testing.T) {
	t.Helper()
	metricsReaderOnce.Do(func() {
		metricsReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricsReader)))
	})
}

func counterSum(t *testing.T, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, metricsReader.Collect(context.Background(), &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// --- Unit, in instancecache ------------------------------------------------

func TestList_PutAppearsAfterWatchDelivers(t *testing.T) {
	c, kv, _ := newTestCache(t)
	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)

	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))

	list := waitForListLen(t, c, acctA, 1)
	require.Equal(t, "i-1", list[0].ID)
}

func TestList_DeleteAndPurgeRemove(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))
	putRecord(t, kv, "i-2", testRecord("i-2", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 2)

	require.NoError(t, kv.Delete(context.Background(), testPrefix+"i-1"))
	list := waitForListLen(t, c, acctA, 1)
	require.Equal(t, "i-2", list[0].ID)

	require.NoError(t, kv.Purge(context.Background(), testPrefix+"i-2"))
	waitForListLen(t, c, acctA, 0)
}

func TestPeriodicResync_RemovesEntryTheWatchNeverToldItAbout(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-real", testRecord("i-real", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	// Simulate exactly what a per-key queue cannot cover: a key is gone from
	// the record space, but nothing was watching when it went, so the live
	// map still shows it.
	c.mu.Lock()
	putInto(c.entries, c.index, "i-ghost", vm.VMFromRecord(testRecord("i-ghost", acctA, vm.StateRunning)))
	c.mu.Unlock()
	waitForListLen(t, c, acctA, 2)

	c.periodicResync(ctx)

	list := waitForListLen(t, c, acctA, 1)
	require.Equal(t, "i-real", list[0].ID)
}

func TestFreshSync_LeaksNothingOnRepeatedFailure(t *testing.T) {
	c, kv, conn := newTestCache(t)
	putGarbage(t, kv, "i-bad")

	ctx := context.Background()
	baseline := conn.nc.NumSubscriptions()

	for range 10 {
		lw := c.openWatcher(ctx)
		require.NotNil(t, lw)
		_, _, _, err := c.snapshotCandidate(ctx)
		require.Error(t, err)
		c.discardWatcher(lw)
		require.Equal(t, baseline, conn.nc.NumSubscriptions())
	}
	require.False(t, c.Ready())
}

func TestFreshSync_BufferOverflowFailsSync(t *testing.T) {
	c, _, conn := newTestCache(t, func(cfg *Config) { cfg.MaxBufferedEvents = 1 })
	ctx := context.Background()
	baseline := conn.nc.NumSubscriptions()

	lw := c.openWatcher(ctx)
	require.NotNil(t, lw)

	c.handleEntry(lw, putEntry(t, "i-1", testRecord("i-1", acctA, vm.StateRunning), 1))
	c.handleEntry(lw, putEntry(t, "i-2", testRecord("i-2", acctA, vm.StateRunning), 2))

	lw.mu.Lock()
	bufErr := lw.bufErr
	lw.mu.Unlock()
	require.ErrorIs(t, bufErr, errBufferOverflow)

	c.discardWatcher(lw)
	require.Equal(t, baseline, conn.nc.NumSubscriptions())
	require.False(t, c.Ready())
}

func TestPeriodicResync_TTLExpiredRecordRemovedWithNoWatchEvent(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	bucket := fmt.Sprintf("instances-ttl-%d", time.Now().UnixNano())
	kv, err := js.CreateKeyValue(context.Background(),
		jetstream.KeyValueConfig{Bucket: bucket, History: 1, TTL: 200 * time.Millisecond})
	require.NoError(t, err)

	c := New(js, Config{
		Bucket:        kvstore.Config{Name: bucket, History: 1, Replicas: 1},
		Prefix:        testPrefix,
		RetryInterval: 20 * time.Millisecond,
	})
	putRecord(t, kv, "i-ttl", testRecord("i-ttl", acctA, vm.StateTerminated))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	time.Sleep(500 * time.Millisecond) // past the bucket TTL; no delete event is ever delivered

	c.mu.RLock()
	_, stillThere := c.entries["i-ttl"]
	c.mu.RUnlock()
	require.True(t, stillThere, "the watch alone must not have removed a TTL-expired key")

	c.periodicResync(ctx)
	waitForListLen(t, c, acctA, 0)
}

func TestPeriodicResync_UndecodableRecordMarksDegraded(t *testing.T) {
	installMetricsReader(t)
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	before := counterSum(t, "mulga.instancecache.resync_failures")
	beforeAge, hadAge := c.resyncAge()
	require.True(t, hadAge)

	putGarbage(t, kv, "i-bad")
	time.Sleep(50 * time.Millisecond)

	c.periodicResync(ctx)

	require.Equal(t, before+1, counterSum(t, "mulga.instancecache.resync_failures"))
	require.True(t, c.Degraded(), "a failed resync must mark the cache degraded")

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.Len(t, list, 1)
	require.Equal(t, "i-1", list[0].ID)

	afterAge, hadAge2 := c.resyncAge()
	require.True(t, hadAge2)
	require.GreaterOrEqual(t, afterAge, beforeAge, "a failed resync must not refresh the last-successful-resync mark")
}

func TestDegraded_ClearedByNextSuccessfulResync(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)
	require.False(t, c.Degraded(), "a cache that has never failed a sync must not report degraded")

	putGarbage(t, kv, "i-bad")
	time.Sleep(50 * time.Millisecond)
	c.periodicResync(ctx)
	require.True(t, c.Degraded())

	require.NoError(t, kv.Delete(context.Background(), testPrefix+"i-bad"))
	time.Sleep(50 * time.Millisecond)
	c.periodicResync(ctx)
	require.False(t, c.Degraded(), "the next successful resync must clear degraded")
}

func TestWatch_UndecodableUpdateLeavesExistingEntry(t *testing.T) {
	installMetricsReader(t)
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	before := counterSum(t, "mulga.instancecache.decode_failures")
	putGarbage(t, kv, "i-1")
	waitFor(t, 2*time.Second, func() bool {
		return counterSum(t, "mulga.instancecache.decode_failures") > before
	})

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.Len(t, list, 1)
	require.Equal(t, vm.StateRunning, list[0].Status,
		"an undecodable watch update must not remove or corrupt the existing entry")
}

func TestList_NotReadyBeforeFirstSync(t *testing.T) {
	c, _, _ := newTestCache(t)
	list, ready := c.List(context.Background(), acctA)
	require.False(t, ready)
	require.Empty(t, list)
}

// --- Fencing, the D2a algorithm --------------------------------------------

func TestFence_BufferedEventAtOrBelowHighWaterDiscarded(t *testing.T) {
	c, _, _ := newTestCache(t)
	entries := map[string]*vm.VM{}
	index := map[string]map[string]struct{}{}
	putInto(entries, index, "i-x", vm.VMFromRecord(testRecord("i-x", acctA, vm.StateRunning)))

	buf := []jetstream.KeyValueEntry{putEntry(t, "i-x", testRecord("i-x", acctA, vm.StateStopped), 5)}
	c.drainBuffered(entries, index, buf, 5)

	require.Equal(t, vm.StateRunning, entries["i-x"].Status,
		"a buffered event at highWater must not overwrite what the snapshot already reflects")
}

func TestFence_PutAboveHighWaterIsPresentAfterwards(t *testing.T) {
	c, _, _ := newTestCache(t)
	entries := map[string]*vm.VM{}
	index := map[string]map[string]struct{}{}

	buf := []jetstream.KeyValueEntry{putEntry(t, "i-y", testRecord("i-y", acctA, vm.StateRunning), 6)}
	c.drainBuffered(entries, index, buf, 5)

	v, ok := entries["i-y"]
	require.True(t, ok)
	require.Equal(t, vm.StateRunning, v.Status)
	require.Contains(t, index[acctA], "i-y")
}

func TestFence_DeleteAboveHighWaterIsAbsentAfterwards(t *testing.T) {
	c, _, _ := newTestCache(t)
	entries := map[string]*vm.VM{}
	index := map[string]map[string]struct{}{}
	putInto(entries, index, "i-z", vm.VMFromRecord(testRecord("i-z", acctA, vm.StateRunning)))

	buf := []jetstream.KeyValueEntry{deleteEntry("i-z", 6)}
	c.drainBuffered(entries, index, buf, 5)

	_, ok := entries["i-z"]
	require.False(t, ok, "a delete above highWater must remove the snapshot's stale copy")
	require.NotContains(t, index[acctA], "i-z")
}

func TestFreshSync_NotReadyUntilBufferDrained(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))
	ctx := context.Background()

	lw := c.openWatcher(ctx)
	require.NotNil(t, lw)
	require.False(t, c.Ready(), "opening the watcher alone must not mark the cache ready")

	entries, index, hw, err := c.snapshotCandidate(ctx)
	require.NoError(t, err)
	require.False(t, c.Ready(), "taking the snapshot alone must not mark the cache ready")

	lw.mu.Lock()
	buf, bufErr := lw.buf, lw.bufErr
	lw.mu.Unlock()
	require.NoError(t, bufErr)
	c.drainBuffered(entries, index, buf, hw)
	require.False(t, c.Ready(), "draining the buffer into a candidate must not itself mark ready")

	c.mu.Lock()
	c.entries, c.index = entries, index
	c.ready = true
	c.mu.Unlock()
	require.True(t, c.Ready())

	c.discardWatcher(lw)
}

func TestPeriodicResync_FailureLeavesLiveWatcherAndMapServing(t *testing.T) {
	c, kv, conn := newTestCache(t)
	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	baseline := conn.nc.NumSubscriptions()
	putGarbage(t, kv, "i-bad")

	for range 10 {
		c.periodicResync(ctx)
		list, ready := c.List(ctx, acctA)
		require.True(t, ready)
		require.Len(t, list, 1)
		require.Equal(t, "i-1", list[0].ID)
		require.Equal(t, baseline, conn.nc.NumSubscriptions(),
			"a periodic resync tees the live watcher rather than opening a new one")
	}
}

func TestPeriodicResync_FailureLosesNothing(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	putGarbage(t, kv, "i-bad") // every future resync now fails against the same bad key

	for i := range 10 {
		id := fmt.Sprintf("i-live-%d", i)
		putRecord(t, kv, id, testRecord(id, acctA, vm.StateRunning))
		waitFor(t, 2*time.Second, func() bool {
			list, _ := c.List(ctx, acctA)
			for _, v := range list {
				if v.ID == id {
					return true
				}
			}
			return false
		})

		c.periodicResync(ctx)

		list, ready := c.List(ctx, acctA)
		require.True(t, ready)
		require.Contains(t, idsOf(list), id)
		require.Contains(t, idsOf(list), "i-seed")
	}

	require.NoError(t, kv.Delete(context.Background(), testPrefix+"i-seed"))
	waitFor(t, 2*time.Second, func() bool {
		list, _ := c.List(ctx, acctA)
		for _, v := range list {
			if v.ID == "i-seed" {
				return false
			}
		}
		return true
	})
}

func TestPeriodicResync_OverflowLosesNothing(t *testing.T) {
	c, kv, _ := newTestCache(t, func(cfg *Config) { cfg.MaxBufferedEvents = 2 })
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	lw := c.getActive()
	require.NotNil(t, lw)
	lw.mu.Lock()
	lw.mode = modeTee
	lw.buf = nil
	lw.bufErr = nil
	lw.mu.Unlock()

	// Three events against a cap of two: the third overflows the buffer, but
	// the tee still applies every one of them to the live map directly.
	for i := range 3 {
		id := fmt.Sprintf("i-x%d", i)
		c.handleEntry(lw, putEntry(t, id, testRecord(id, acctA, vm.StateRunning), uint64(100+i)))
	}
	lw.mu.Lock()
	bufErr := lw.bufErr
	lw.mu.Unlock()
	require.ErrorIs(t, bufErr, errBufferOverflow)

	_, _, _, err := c.snapshotCandidate(ctx)
	require.NoError(t, err)
	// The candidate is discarded on overflow; goLive resets the watcher for
	// normal live service without installing it.
	c.goLive(lw)

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	ids := idsOf(list)
	require.Contains(t, ids, "i-seed")
	for i := range 3 {
		require.Contains(t, ids, fmt.Sprintf("i-x%d", i))
	}
}

func TestPeriodicResync_TwentyConsecutiveSuccessesOneWatcher(t *testing.T) {
	c, kv, conn := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	initial := c.getActive()
	baseline := conn.nc.NumSubscriptions()

	for range 20 {
		c.periodicResync(ctx)
		require.Equal(t, baseline, conn.nc.NumSubscriptions())
		require.Same(t, initial, c.getActive(), "the subscription must be the same one throughout")
	}

	// Land an update precisely during a swap: after the snapshot is taken but
	// before the candidate is installed and the watcher goes back live.
	lw := c.getActive()
	lw.mu.Lock()
	lw.mode = modeTee
	lw.buf = nil
	lw.bufErr = nil
	lw.mu.Unlock()

	entries, index, hw, err := c.snapshotCandidate(ctx)
	require.NoError(t, err)

	updated := putEntry(t, "i-seed", testRecord("i-seed", acctA, vm.StateStopped), hw+1)
	c.handleEntry(lw, updated)

	lw.mu.Lock()
	buf, bufErr := lw.buf, lw.bufErr
	lw.mu.Unlock()
	require.NoError(t, bufErr)
	c.drainBuffered(entries, index, buf, hw)

	c.mu.Lock()
	c.entries, c.index = entries, index
	c.mu.Unlock()
	c.goLive(lw)

	list, _ := c.List(ctx, acctA)
	require.Len(t, list, 1, "the update must appear exactly once")
	require.Equal(t, vm.StateStopped, list[0].Status)
}

func TestReplaceWatcher_OldStoppedAndJoinedBeforeNewGoesLive(t *testing.T) {
	c, kv, conn := newTestCache(t)
	putRecord(t, kv, "i-1", testRecord("i-1", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	oldLw := c.getActive()
	baseline := conn.nc.NumSubscriptions()

	require.NoError(t, oldLw.kw.Stop()) // simulate the connection dropping this watcher

	waitFor(t, 2*time.Second, func() bool {
		select {
		case <-oldLw.done:
		default:
			return false
		}
		next := c.getActive()
		return next != nil && next != oldLw
	})

	select {
	case <-oldLw.done:
	default:
		t.Fatal("old watcher must be joined once replacement completes")
	}

	waitFor(t, 2*time.Second, func() bool { return conn.nc.NumSubscriptions() == baseline })

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.Len(t, list, 1)
	require.Equal(t, "i-1", list[0].ID)

	putRecord(t, kv, "i-2", testRecord("i-2", acctA, vm.StateRunning))
	waitForListLen(t, c, acctA, 2)
}

// The four tests below drive the real freshSync/periodicResync code paths
// end to end, landing a write via postSnapshotHook in the exact window D2a
// exists to cover: after Snapshot has already read the record space, before
// the buffer it fences against is drained. They call drainBuffered nowhere
// themselves; unlike the direct-call fence tests above, they fail if the
// watcher is not already buffering by the time the snapshot completes. A
// short sleep inside the hook gives the already-open, already-subscribed
// watcher time to receive and process the write before the sync continues,
// so the event deterministically lands inside the window rather than racing
// to land there.

func TestFence_PutDuringInitialSnapshot_SurvivesInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	c.postSnapshotHook = func() {
		putRecord(t, kv, "i-new", testRecord("i-new", acctA, vm.StateRunning))
		time.Sleep(100 * time.Millisecond)
	}

	lw := c.freshSync(context.Background())
	require.NotNil(t, lw)
	t.Cleanup(func() { c.discardWatcher(lw) })

	list, ready := c.List(context.Background(), acctA)
	require.True(t, ready)
	require.ElementsMatch(t, []string{"i-seed", "i-new"}, idsOf(list),
		"a put landing during the initial snapshot must survive into the installed candidate")
}

func TestFence_DeleteDuringInitialSnapshot_RemovedFromInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))
	putRecord(t, kv, "i-doomed", testRecord("i-doomed", acctA, vm.StateRunning))

	c.postSnapshotHook = func() {
		require.NoError(t, kv.Delete(context.Background(), testPrefix+"i-doomed"))
		time.Sleep(100 * time.Millisecond)
	}

	lw := c.freshSync(context.Background())
	require.NotNil(t, lw)
	t.Cleanup(func() { c.discardWatcher(lw) })

	list, ready := c.List(context.Background(), acctA)
	require.True(t, ready)
	require.Equal(t, []string{"i-seed"}, idsOf(list),
		"a delete landing during the initial snapshot must not be overwritten by the snapshot's stale copy")
}

func TestFence_PutDuringPeriodicResync_SurvivesInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	c.postSnapshotHook = func() {
		putRecord(t, kv, "i-new", testRecord("i-new", acctA, vm.StateRunning))
		time.Sleep(100 * time.Millisecond)
	}
	c.periodicResync(ctx)
	c.postSnapshotHook = nil

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.ElementsMatch(t, []string{"i-seed", "i-new"}, idsOf(list),
		"a put landing during a periodic resync's snapshot must survive into the installed candidate")
}

func TestFence_DeleteDuringPeriodicResync_RemovedFromInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))
	putRecord(t, kv, "i-doomed", testRecord("i-doomed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 2)

	c.postSnapshotHook = func() {
		require.NoError(t, kv.Delete(context.Background(), testPrefix+"i-doomed"))
		time.Sleep(100 * time.Millisecond)
	}
	c.periodicResync(ctx)
	c.postSnapshotHook = nil

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.Equal(t, []string{"i-seed"}, idsOf(list),
		"a delete landing during a periodic resync's snapshot must not be overwritten by the snapshot's stale copy")
}

// --- The account index ------------------------------------------------------

func TestList_ReturnsOnlyThatAccountsInstances(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-a", testRecord("i-a", acctA, vm.StateRunning))
	putRecord(t, kv, "i-b", testRecord("i-b", acctB, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)
	waitForListLen(t, c, acctB, 1)

	listA, _ := c.List(ctx, acctA)
	require.Equal(t, []string{"i-a"}, idsOf(listA))

	listB, _ := c.List(ctx, acctB)
	require.Equal(t, []string{"i-b"}, idsOf(listB))
}

func TestList_GlobalDoesNotSeeOtherAccountsInstances(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-cust", testRecord("i-cust", acctA, vm.StateRunning))
	putRecord(t, kv, "i-global", testRecord("i-global", utils.GlobalAccountID, vm.StateRunning))
	putRecord(t, kv, "i-legacy", testRecord("i-legacy", "", vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitFor(t, 2*time.Second, func() bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return len(c.entries) == 3
	})

	listGlobal, ready := c.List(ctx, utils.GlobalAccountID)
	require.True(t, ready)
	require.ElementsMatch(t, []string{"i-global", "i-legacy"}, idsOf(listGlobal))

	listCust, _ := c.List(ctx, acctA)
	require.Equal(t, []string{"i-cust"}, idsOf(listCust))
}

func TestList_VisibilityAppliedAfterIndexLookup(t *testing.T) {
	c, _, _ := newTestCache(t)

	// Deliberately mis-file: the index says i-misfiled belongs to acctA, but
	// the record itself is owned by acctB. IsInstanceVisibleToCaller must
	// still reject it at the List seam.
	c.mu.Lock()
	c.entries["i-misfiled"] = vm.VMFromRecord(testRecord("i-misfiled", acctB, vm.StateRunning))
	addToIndex(c.index, acctA, "i-misfiled")
	c.ready = true
	c.mu.Unlock()

	list, ready := c.List(context.Background(), acctA)
	require.True(t, ready)
	require.Empty(t, list)
}
