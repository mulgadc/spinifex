// internal watcher and lifecycle state directly (there is no exported
// surface for "one live watcher" or "the candidate before install"), and the
// account-index defence-in-depth test deliberately mis-files a record by
// writing straight into the unexported maps.
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

func TestSync_LeaksNothingOnRepeatedSnapshotFailure(t *testing.T) {
	c, kv, conn := newTestCache(t)
	putGarbage(t, kv, "i-bad")

	ctx := t.Context()
	baseline := conn.nc.NumSubscriptions()
	go c.Run(ctx)

	// The initial sync's snapshot fails on the garbage record every retry, so
	// the replay watcher is never opened: nothing persists to leak. Snapshot
	// itself briefly opens and closes its own probe watcher on every attempt,
	// so the assertion is that the count always settles back to baseline,
	// not that it never blips.
	for range 10 {
		time.Sleep(20 * time.Millisecond)
		waitFor(t, time.Second, func() bool { return conn.nc.NumSubscriptions() == baseline })
	}
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

// --- Fencing: replay closes the snapshot-to-watch window --------------------
//
// Each test lands a write in the exact window the fence has to cover: after
// Snapshot has already read the record space, before the new watcher (which
// resumes from the snapshot's high-water mark) is opened. There is no
// buffering machinery to race against any more, so the write happens as an
// ordinary step between two ordinary calls.

func TestFence_PutDuringInitialSnapshot_SurvivesInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	entries, index, hw, err := c.snapshotCandidate(context.Background())
	require.NoError(t, err)

	putRecord(t, kv, "i-new", testRecord("i-new", acctA, vm.StateRunning))

	watchCtx := t.Context()
	kw, err := c.store.WatchFrom(watchCtx, c.filter, hw+1)
	require.NoError(t, err)
	defer func() { _ = kw.Stop() }()

	require.NoError(t, c.replay(watchCtx, kw, entries, index))

	require.Len(t, entries, 2)
	require.Contains(t, entries, "i-seed")
	require.Contains(t, entries, "i-new",
		"a put landing after the snapshot but before the watcher opened must be replayed")
}

func TestFence_DeleteDuringInitialSnapshot_RemovedFromInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))
	putRecord(t, kv, "i-doomed", testRecord("i-doomed", acctA, vm.StateRunning))

	entries, index, hw, err := c.snapshotCandidate(context.Background())
	require.NoError(t, err)
	require.Len(t, entries, 2)

	require.NoError(t, kv.Delete(context.Background(), testPrefix+"i-doomed"))

	watchCtx := t.Context()
	kw, err := c.store.WatchFrom(watchCtx, c.filter, hw+1)
	require.NoError(t, err)
	defer func() { _ = kw.Stop() }()

	require.NoError(t, c.replay(watchCtx, kw, entries, index))

	require.Len(t, entries, 1)
	require.Contains(t, entries, "i-seed")
	require.NotContains(t, entries, "i-doomed",
		"a delete landing after the snapshot but before the watcher opened must not survive as the snapshot's stale copy")
	require.NotContains(t, index[acctA], "i-doomed")
}

func TestFence_PutDuringPeriodicResync_SurvivesInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	oldLw := c.getActive()
	require.NotNil(t, oldLw)

	entries, index, hw, err := c.snapshotCandidate(ctx)
	require.NoError(t, err)

	putRecord(t, kv, "i-new", testRecord("i-new", acctA, vm.StateRunning))

	watchCtx, cancel := context.WithCancel(ctx)
	kw, err := c.store.WatchFrom(watchCtx, c.filter, hw+1)
	require.NoError(t, err)
	require.NoError(t, c.replay(watchCtx, kw, entries, index))

	lw := &liveWatcher{kw: kw, ctx: watchCtx, cancel: cancel, done: make(chan struct{})}
	c.installSync(lw, entries, index, oldLw)

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.ElementsMatch(t, []string{"i-seed", "i-new"}, idsOf(list),
		"a put landing after a periodic resync's snapshot but before the new watcher opened must be replayed")
}

func TestFence_DeleteDuringPeriodicResync_RemovedFromInstall(t *testing.T) {
	c, kv, _ := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))
	putRecord(t, kv, "i-doomed", testRecord("i-doomed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 2)

	oldLw := c.getActive()
	require.NotNil(t, oldLw)

	entries, index, hw, err := c.snapshotCandidate(ctx)
	require.NoError(t, err)

	require.NoError(t, kv.Delete(context.Background(), testPrefix+"i-doomed"))

	watchCtx, cancel := context.WithCancel(ctx)
	kw, err := c.store.WatchFrom(watchCtx, c.filter, hw+1)
	require.NoError(t, err)
	require.NoError(t, c.replay(watchCtx, kw, entries, index))

	lw := &liveWatcher{kw: kw, ctx: watchCtx, cancel: cancel, done: make(chan struct{})}
	c.installSync(lw, entries, index, oldLw)

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.Equal(t, []string{"i-seed"}, idsOf(list),
		"a delete landing after a periodic resync's snapshot but before the new watcher opened must not be overwritten by the snapshot's stale copy")
}

// --- Lifecycle ---------------------------------------------------------------

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
			"a failed periodic resync must not leave a stray subscription behind")
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

func TestPeriodicResync_TwentyConsecutiveSuccessesOneWatcherEach(t *testing.T) {
	c, kv, conn := newTestCache(t)
	putRecord(t, kv, "i-seed", testRecord("i-seed", acctA, vm.StateRunning))

	ctx := t.Context()
	go c.Run(ctx)
	waitForReady(t, c)
	waitForListLen(t, c, acctA, 1)

	baseline := conn.nc.NumSubscriptions()

	// Each periodic resync opens a fresh watcher and retires the last one, so
	// the subscription count stays flat even though the watcher identity
	// changes every time.
	prev := c.getActive()
	for range 20 {
		c.periodicResync(ctx)
		require.Equal(t, baseline, conn.nc.NumSubscriptions())
		cur := c.getActive()
		require.NotSame(t, prev, cur, "each successful periodic resync installs a new watcher")
		prev = cur
	}

	list, ready := c.List(ctx, acctA)
	require.True(t, ready)
	require.Equal(t, []string{"i-seed"}, idsOf(list))
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
