//test:in-package: exercises the unexported digest helpers against a real clustered
// store, which the cobra command only reaches through a live node.

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kvServer runs a standalone JetStream server on storeParent and returns a KV
// bucket on it plus a stop function.
func kvServer(t *testing.T, storeParent, bucket string) (jetstream.KeyValue, func()) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "kv-test", JetStream: true, StoreDir: storeParent,
		DontListen: true, NoLog: true, NoSigs: true,
	})
	require.NoError(t, err)
	ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second))
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	require.NoError(t, err)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: bucket, History: 5})
	require.NoError(t, err)
	return kv, func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
	}
}

func digestOf(t *testing.T, storeDir string, withSeqs bool) []streamDigest {
	t.Helper()
	work := t.TempDir()
	_, metas, err := copyStreamTree(storeDir, filepath.Join(work, "jetstream"))
	require.NoError(t, err)
	d, err := digestStore(context.Background(), work, nil, withSeqs, metas)
	require.NoError(t, err)
	return d
}

func TestKVDigest(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	kv, stop := kvServer(t, parent, "test")
	for _, kvp := range [][2]string{{"a", "1"}, {"b", "2"}, {"a", "3"}} {
		_, err := kv.PutString(ctx, kvp[0], kvp[1])
		require.NoError(t, err)
	}
	require.NoError(t, kv.Delete(ctx, "b"))
	store := filepath.Join(parent, "jetstream")

	// Copied while the server is live, exactly as it runs on a node.
	live := digestOf(t, store, true)
	stop()

	require.Len(t, live, 1)
	assert.Equal(t, "KV_test", live[0].Name)
	assert.Equal(t, uint64(1), live[0].FirstSeq)
	assert.Equal(t, uint64(4), live[0].LastSeq)
	assert.Equal(t, uint64(4), live[0].Msgs)
	require.Len(t, live[0].Seqs, 4)

	t.Run("stable across reads and a stopped server", func(t *testing.T) {
		again := digestOf(t, store, false)
		assert.Equal(t, live[0].Digest, again[0].Digest)
	})

	t.Run("source store is left untouched", func(t *testing.T) {
		_, err := os.Stat(filepath.Join(store, "$G", "streams", "KV_test"))
		assert.NoError(t, err)
	})

	// Same keys and values written separately differ in timestamps, which is
	// exactly what separates two nodes' pre-formation writes.
	t.Run("independently written store differs", func(t *testing.T) {
		other := t.TempDir()
		kv2, stop2 := kvServer(t, other, "test")
		for _, kvp := range [][2]string{{"a", "1"}, {"b", "2"}, {"a", "3"}} {
			_, err := kv2.PutString(ctx, kvp[0], kvp[1])
			require.NoError(t, err)
		}
		require.NoError(t, kv2.Delete(ctx, "b"))
		stop2()
		d := digestOf(t, filepath.Join(other, "jetstream"), true)
		assert.NotEqual(t, live[0].Digest, d[0].Digest)

		// Two stores adopted as replicas of one stream: same subjects and
		// sequences, different content, which compare must call divergent.
		res, err := compareNodeDigests([]nodeDigest{
			{Host: "node1", CopiedAt: time.Now(), Streams: live},
			{Host: "node2", CopiedAt: time.Now(), Streams: d},
		})
		require.NoError(t, err)
		require.True(t, res.divergent())
		require.Len(t, res.Streams, 1)
		assert.Contains(t, strings.Join(res.Streams[0].Problems, "\n"), "seq 1 $KV.test.a holds different content")
	})

	t.Run("report lines", func(t *testing.T) {
		var buf bytes.Buffer
		writeDigests(&buf, "node1", store, time.Unix(0, 0), []string{"ACC/S"}, live)
		out := buf.String()
		assert.Contains(t, out, "# host=node1 ")
		assert.Contains(t, out, "# skipped account stream (not read): ACC/S")
		assert.Contains(t, out, "KV_test\tfirst=1\tlast=4\tmsgs=4\tsha256="+live[0].Digest)
		assert.Equal(t, 4, strings.Count(out, "KV_test\tseq="))
	})
}

func TestKVDigestStreamFilterAndDeletedSeq(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	// The KV bucket is only there to be excluded by the stream filter.
	_, stop := kvServer(t, parent, "other")
	stop()

	// KV streams deny message deletes, so an interior gap needs a plain stream.
	ns, err := server.NewServer(&server.Options{JetStream: true, StoreDir: parent, DontListen: true, NoLog: true, NoSigs: true})
	require.NoError(t, err)
	ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second))
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	require.NoError(t, err)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	s, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ONE", Subjects: []string{"one.>"}})
	require.NoError(t, err)
	for _, v := range []string{"v1", "v2", "v3"} {
		_, err = js.Publish(ctx, "one.k", []byte(v))
		require.NoError(t, err)
	}
	require.NoError(t, s.DeleteMsg(ctx, 2))
	nc.Close()
	ns.Shutdown()
	ns.WaitForShutdown()

	work := t.TempDir()
	_, metas, err := copyStreamTree(filepath.Join(parent, "jetstream"), filepath.Join(work, "jetstream"))
	require.NoError(t, err)
	d, err := digestStore(ctx, work, []string{"ONE"}, true, metas)
	require.NoError(t, err)
	require.Len(t, d, 1)
	assert.Equal(t, "ONE", d[0].Name)
	assert.Equal(t, uint64(2), d[0].Msgs)
	require.Len(t, d[0].Seqs, 3)
	assert.Equal(t, seqDigest{Seq: 2, Hash: "deleted"}, d[0].Seqs[1])
	for _, i := range []int{0, 2} {
		assert.Equal(t, "one.k", d[0].Seqs[i].Subject)
		assert.False(t, d[0].Seqs[i].Time.IsZero())
	}
	assert.Equal(t, "limits", d[0].Retention)
	assert.Equal(t, 1, d[0].Replicas)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A replicated stream's meta.inf carries num_replicas > 1, which a standalone
// server refuses to load; this is the case every production node presents.
func TestKVDigestClusteredReplicas(t *testing.T) {
	ctx := context.Background()
	const n = 3
	ports := make([]int, n)
	routes := make([]string, n)
	for i := range ports {
		ports[i] = freePort(t)
		routes[i] = fmt.Sprintf("nats://127.0.0.1:%d", ports[i])
	}
	stores := make([]string, n)
	servers := make([]*server.Server, n)
	for i := range servers {
		stores[i] = t.TempDir()
		ns, err := server.NewServer(&server.Options{
			ServerName: fmt.Sprintf("s%d", i+1), JetStream: true, StoreDir: stores[i],
			Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
			Cluster: server.ClusterOpts{Name: "digest", Host: "127.0.0.1", Port: ports[i]},
			Routes:  server.RoutesFromStr(strings.Join(routes, ",")),
		})
		require.NoError(t, err)
		ns.Start()
		servers[i] = ns
	}
	t.Cleanup(func() {
		for _, ns := range servers {
			ns.Shutdown()
			ns.WaitForShutdown()
		}
	})

	nc, err := nats.Connect("", nats.InProcessServer(servers[0]))
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	var kv jetstream.KeyValue
	require.Eventually(t, func() bool {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "rep", History: 3, Replicas: n, TTL: time.Hour})
		return err == nil
	}, 30*time.Second, 250*time.Millisecond, "clustered KV create: %v", err)
	for i := range 5 {
		_, err := kv.PutString(ctx, fmt.Sprintf("k%d", i%2), fmt.Sprintf("v%d", i))
		require.NoError(t, err)
	}
	s, err := js.Stream(ctx, "KV_rep")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		info, err := s.Info(ctx)
		if err != nil || info.Cluster == nil || len(info.Cluster.Replicas) != n-1 {
			return false
		}
		for _, r := range info.Cluster.Replicas {
			if !r.Current || r.Lag != 0 {
				return false
			}
		}
		return true
	}, 30*time.Second, 250*time.Millisecond)

	// Current means applied, not flushed; shutdown flushes every block to disk.
	nc.Close()
	for _, ns := range servers {
		ns.Shutdown()
		ns.WaitForShutdown()
	}

	var first string
	var nodes []nodeDigest
	for i, store := range stores {
		d := digestOf(t, filepath.Join(store, "jetstream"), true)
		require.Len(t, d, 1, "replica %d", i+1)
		assert.Equal(t, uint64(5), d[0].Msgs, "replica %d", i+1)
		assert.Equal(t, n, d[0].Replicas, "live replica count survives the standalone rewrite")
		assert.Equal(t, time.Hour, d[0].MaxAge)
		nodes = append(nodes, nodeDigest{Host: fmt.Sprintf("s%d", i+1), CopiedAt: time.Now(), Streams: d})
		if i == 0 {
			first = d[0].Digest
			continue
		}
		assert.Equal(t, first, d[0].Digest, "replica %d matches replica 1", i+1)
	}
	res, err := compareNodeDigests(nodes)
	require.NoError(t, err)
	assert.False(t, res.divergent(), "%+v", res)
}

// Recovery applies MaxAge, so without the rewrite an expired bucket copy
// would read as empty and mask whatever the replica holds.
func TestKVDigestIgnoresExpiry(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	ns, err := server.NewServer(&server.Options{JetStream: true, StoreDir: parent, DontListen: true, NoLog: true, NoSigs: true})
	require.NoError(t, err)
	ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second))
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	require.NoError(t, err)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "ttl", TTL: time.Second})
	require.NoError(t, err)
	_, err = kv.PutString(ctx, "k", "v")
	require.NoError(t, err)

	work := t.TempDir()
	_, metas, err := copyStreamTree(filepath.Join(parent, "jetstream"), filepath.Join(work, "jetstream"))
	require.NoError(t, err)
	nc.Close()
	ns.Shutdown()
	ns.WaitForShutdown()

	time.Sleep(1500 * time.Millisecond)
	d, err := digestStore(ctx, work, nil, false, metas)
	require.NoError(t, err)
	require.Len(t, d, 1)
	assert.Equal(t, uint64(1), d[0].Msgs)
}

func TestCopyStreamTree(t *testing.T) {
	store := seedStore(t, "KV_a")
	require.NoError(t, os.MkdirAll(filepath.Join(store, "$G", "streams", "KV_a", "obs", "c1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(store, "$G", "streams", "KV_a", "obs", "c1", "o.dat"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(store, "ACME", "streams", "ORDERS"), 0o755))

	dst := filepath.Join(t.TempDir(), "jetstream")
	skipped, _, err := copyStreamTree(store, dst)
	require.NoError(t, err)
	assert.Equal(t, []string{"ACME/ORDERS"}, skipped)

	_, err = os.Stat(filepath.Join(dst, "$G", "streams", "KV_a", "msgs", "1.blk"))
	assert.NoError(t, err, "message blocks are copied")
	_, err = os.Stat(filepath.Join(dst, "$G", "streams", "KV_a", "obs"))
	assert.True(t, os.IsNotExist(err), "consumer state is left out")

	_, _, err = copyStreamTree(filepath.Join(t.TempDir(), "absent"), dst)
	assert.Error(t, err)
}
