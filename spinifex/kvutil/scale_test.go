package kvutil_test

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// The two shapes under comparison, and the whole question this file exists to
// answer: is a bucket per tenant cheaper or dearer than one bucket holding
// every tenant's keys?
//
// Nothing here asserts a threshold. A scale measurement that fails the build
// on a number would be measuring the CI runner, so these print and the reader
// decides. The assertions are only that the work actually happened.

// bucketCounts is the sweep. It stops at 2000 because the interesting shape —
// whether cost per bucket is flat or rising — is visible well before the
// projected ceiling, and running to 5000 on three in-process servers costs more
// time than the extra point is worth.
var bucketCounts = []int{50, 100, 250, 500, 1000, 2000}

// seedKeysPerBucket is how much state each bucket carries. Small on purpose:
// the question is what a bucket costs, not what a key costs, and holding it
// constant keeps the bucket count the only variable.
const seedKeysPerBucket = 10

// TestKVScale_BucketPerTenant measures what a bucket per tenant costs as the
// tenant count rises: time to create, heap held, and how long the replica audit
// and a restart take once they exist.
//
// The audit figure is the one with an operator behind it. RaiseAllBucketReplicas
// calls AuditBucketReplicas at daemon start, and the audit is two sequential
// round trips per bucket, so this number sits between a node booting and that
// node being useful.
func TestKVScale_BucketPerTenant(t *testing.T) {
	requireScaleEnabled(t)

	clustersize.ResetForTest()
	clustersize.DeclareForTest(t, 3)

	type row struct {
		buckets  int
		create   time.Duration
		perOne   time.Duration
		settle   time.Duration
		heapMB   float64
		audit    time.Duration
		rejoin   time.Duration
		routines int
	}
	var rows []row

	for _, count := range bucketCounts {
		t.Run(fmt.Sprintf("buckets=%d", count), func(t *testing.T) {
			c := startNATSCluster(t, 3)
			ctx := t.Context()

			baseline := settledHeapMB()

			start := time.Now()
			for i := range count {
				name := fmt.Sprintf("scale-tenant-%06d", i)
				kv, err := kvutil.GetOrCreateBucket(ctx, c.js, name, 1)
				if err != nil {
					t.Fatalf("create bucket %d of %d: %v", i, count, err)
				}
				// Seeded so the restart below has something to recover. An empty
				// stream rejoins with nothing to replay, which would make the
				// recovery figure a measure of the Raft handshake alone.
				for k := range seedKeysPerBucket {
					if _, err := kv.Put(ctx, fmt.Sprintf("k%02d", k), []byte("{}")); err != nil {
						t.Fatalf("seed bucket %d key %d: %v", i, k, err)
					}
				}
			}
			create := time.Since(start)

			// Creation returns once the meta layer has accepted the stream, so
			// the cluster is still placing replicas afterwards. Everything below
			// measures a settled cluster rather than one mid-placement.
			start = time.Now()
			c.waitStreamsCurrent(t, count, 10*time.Minute)
			settle := time.Since(start)

			held := settledHeapMB() - baseline

			start = time.Now()
			reports, err := kvutil.AuditBucketReplicas(ctx, c.js)
			require.NoError(t, err, "audit %d buckets", count)
			audit := time.Since(start)
			require.Lenf(t, reports, count, "audit saw %d buckets, expected %d", len(reports), count)

			// One server restarted is the event an operator actually causes —
			// a deploy, a reboot — and the cost of it is what N Raft groups
			// charge for rejoining, which is the figure the stream count drives.
			last := len(c.servers) - 1
			c.servers[last].Shutdown()
			start = time.Now()
			c.start(t, last)
			c.waitStreamsCurrent(t, count, 10*time.Minute)
			rejoin := time.Since(start)

			rows = append(rows, row{
				buckets: count, create: create, perOne: create / time.Duration(count),
				settle: settle, heapMB: held, audit: audit, rejoin: rejoin,
				routines: runtime.NumGoroutine(),
			})
		})
	}

	reportTable(t, fmt.Sprintf("bucket per tenant, %d keys each, 3-node cluster, R3", seedKeysPerBucket),
		[]string{"buckets", "create", "per-bucket", "settle", "heap MB", "audit", "restart rejoin", "goroutines"},
		func() [][]string {
			out := make([][]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, []string{
					strconv.Itoa(r.buckets),
					r.create.Round(time.Millisecond).String(),
					r.perOne.Round(time.Microsecond).String(),
					r.settle.Round(time.Millisecond).String(),
					fmt.Sprintf("%.1f", r.heapMB),
					r.audit.Round(time.Millisecond).String(),
					r.rejoin.Round(time.Millisecond).String(),
					strconv.Itoa(r.routines),
				})
			}
			return out
		}())
}

// TestKVScale_SharedBucket is the other arm: one bucket holding every tenant's
// keys, which is what the consolidation would produce.
//
// It measures the two read paths side by side, because that comparison is the
// whole argument. The codebase today lists a whole bucket and filters in Go;
// with every tenant in one bucket that becomes a full scan, and the filtered
// subject list is what makes the cost follow the answer instead of the store.
func TestKVScale_SharedBucket(t *testing.T) {
	requireScaleEnabled(t)

	clustersize.ResetForTest()
	clustersize.DeclareForTest(t, 3)

	// Keys per tenant is held constant so the variable is tenant count alone.
	const keysPerTenant = 20

	type row struct {
		tenants  int
		keys     int
		write    time.Duration
		heapMB   float64
		scanAll  time.Duration
		filtered time.Duration
	}
	var rows []row

	for _, tenants := range bucketCounts {
		t.Run(fmt.Sprintf("tenants=%d", tenants), func(t *testing.T) {
			c := startNATSCluster(t, 3)
			ctx := t.Context()

			baseline := settledHeapMB()

			kv, err := kvutil.GetOrCreateBucket(ctx, c.js, "scale-shared", 1)
			require.NoError(t, err)

			start := time.Now()
			for tenant := range tenants {
				account := fmt.Sprintf("%012d", tenant)
				for k := range keysPerTenant {
					// The layout the consolidation proposes: account first, dot
					// separated, so the account is its own subject token.
					key := fmt.Sprintf("%s.clusters.c%04d.meta", account, k)
					if _, err := kv.Put(ctx, key, []byte("{}")); err != nil {
						t.Fatalf("put %s: %v", key, err)
					}
				}
			}
			write := time.Since(start)

			c.waitStreamsCurrent(t, 1, time.Minute)
			held := settledHeapMB() - baseline

			// The account whose keys both read paths are asked for. The last
			// one, because a filtered list that secretly scans would show its
			// cost most clearly on the key furthest from the start.
			target := fmt.Sprintf("%012d", tenants-1)

			start = time.Now()
			scanned := scanAndFilter(ctx, t, kv, target+".")
			scanAll := time.Since(start)

			start = time.Now()
			matched := listFiltered(ctx, t, kv, target+".>")
			filtered := time.Since(start)

			require.Equalf(t, keysPerTenant, scanned,
				"whole-bucket scan found %d keys for %s, expected %d", scanned, target, keysPerTenant)
			require.Equalf(t, keysPerTenant, matched,
				"filtered list found %d keys for %s, expected %d", matched, target, keysPerTenant)

			rows = append(rows, row{
				tenants: tenants, keys: tenants * keysPerTenant,
				write: write, heapMB: held, scanAll: scanAll, filtered: filtered,
			})
		})
	}

	reportTable(t, fmt.Sprintf("one shared bucket, %d keys per tenant, 3-node cluster, R3", keysPerTenant),
		[]string{"tenants", "total keys", "write", "heap MB", "whole-bucket scan", "filtered list"},
		func() [][]string {
			out := make([][]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, []string{
					strconv.Itoa(r.tenants),
					strconv.Itoa(r.keys),
					r.write.Round(time.Millisecond).String(),
					fmt.Sprintf("%.1f", r.heapMB),
					r.scanAll.Round(time.Microsecond).String(),
					r.filtered.Round(time.Microsecond).String(),
				})
			}
			return out
		}())
}

// scanAndFilter is what the codebase does today: list every key in the bucket
// and keep the ones with the right prefix. Its cost is the store's size.
func scanAndFilter(ctx context.Context, tb testing.TB, kv jetstream.KeyValue, prefix string) int {
	tb.Helper()
	keys, err := kv.Keys(ctx)
	require.NoError(tb, err, "whole-bucket Keys")
	found := 0
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			found++
		}
	}
	return found
}

// listFiltered asks the server for one account's keys. Its cost should be the
// answer's size, which is the property the consolidation has to preserve.
func listFiltered(ctx context.Context, tb testing.TB, kv jetstream.KeyValue, filter string) int {
	tb.Helper()
	lister, err := kv.ListKeysFiltered(ctx, filter)
	require.NoErrorf(tb, err, "filtered list %q", filter)
	found := 0
	for range lister.Keys() {
		found++
	}
	return found
}

// settledHeapMB is the live heap in megabytes, after collection. The embedded
// servers share this heap, so the delta across a creation loop is their
// retained cost.
//
// Two collections, because the first frees the previous subtest's cluster and
// the second then measures what is actually reachable. One pass leaves enough
// of the last teardown in the figure to make the delta negative.
func settledHeapMB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1024 * 1024)
}

// reportTable prints a measurement table through t.Log, so it lands in the test
// output with the run that produced it rather than in a file nobody keeps.
func reportTable(tb testing.TB, title string, header []string, rows [][]string) {
	tb.Helper()
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			widths[i] = max(widths[i], len(cell))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", title)
	writeRow := func(cells []string) {
		for i, cell := range cells {
			fmt.Fprintf(&b, "  %-*s", widths[i], cell)
		}
		b.WriteByte('\n')
	}
	writeRow(header)
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = strings.Repeat("-", widths[i])
	}
	writeRow(sep)
	for _, r := range rows {
		writeRow(r)
	}
	tb.Log(b.String())
}
