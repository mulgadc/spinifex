package cmd

import (
	"bytes"
	"context"
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
	_, err := copyStreamTree(storeDir, filepath.Join(work, "jetstream"))
	require.NoError(t, err)
	d, err := digestStore(context.Background(), work, nil, withSeqs)
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
		d := digestOf(t, filepath.Join(other, "jetstream"), false)
		assert.NotEqual(t, live[0].Digest, d[0].Digest)
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
	_, err = copyStreamTree(filepath.Join(parent, "jetstream"), filepath.Join(work, "jetstream"))
	require.NoError(t, err)
	d, err := digestStore(ctx, work, []string{"ONE"}, true)
	require.NoError(t, err)
	require.Len(t, d, 1)
	assert.Equal(t, "ONE", d[0].Name)
	assert.Equal(t, uint64(2), d[0].Msgs)
	assert.Equal(t, []seqDigest{{1, d[0].Seqs[0].Hash}, {2, "deleted"}, {3, d[0].Seqs[2].Hash}}, d[0].Seqs)
}

func TestCopyStreamTree(t *testing.T) {
	store := seedStore(t, "KV_a")
	require.NoError(t, os.MkdirAll(filepath.Join(store, "$G", "streams", "KV_a", "obs", "c1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(store, "$G", "streams", "KV_a", "obs", "c1", "o.dat"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(store, "ACME", "streams", "ORDERS"), 0o755))

	dst := filepath.Join(t.TempDir(), "jetstream")
	skipped, err := copyStreamTree(store, dst)
	require.NoError(t, err)
	assert.Equal(t, []string{"ACME/ORDERS"}, skipped)

	_, err = os.Stat(filepath.Join(dst, "$G", "streams", "KV_a", "msgs", "1.blk"))
	assert.NoError(t, err, "message blocks are copied")
	_, err = os.Stat(filepath.Join(dst, "$G", "streams", "KV_a", "obs"))
	assert.True(t, os.IsNotExist(err), "consumer state is left out")

	_, err = copyStreamTree(filepath.Join(t.TempDir(), "absent"), dst)
	assert.Error(t, err)
}
