package jetstream

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKVRemovedMatchesKVGet pins the markers nats.go's KV Get treats as an
// absent key, including server-written TTL and purge markers.
func TestKVRemovedMatchesKVGet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		value   string
		removed bool
	}{
		{"put", "", "", false},
		{"delete", "KV-Operation", "DEL", true},
		{"purge", "KV-Operation", "PURGE", true},
		{"unknown operation", "KV-Operation", "PUT", false},
		{"max age marker", jsapi.MarkerReasonHeader, "MaxAge", true},
		{"purge marker", jsapi.MarkerReasonHeader, "Purge", true},
		{"remove marker", jsapi.MarkerReasonHeader, "Remove", true},
		{"other marker", jsapi.MarkerReasonHeader, "Other", false},
	} {
		h := nats.Header{}
		if tc.key != "" {
			h.Set(tc.key, tc.value)
		}
		assert.Equal(t, tc.removed, kvRemoved(h), tc.name)
	}
	h := nats.Header{}
	h.Set("KV-Operation", "PUT")
	h.Set(jsapi.MarkerReasonHeader, "Remove")
	assert.False(t, kvRemoved(h), "an operation header takes precedence, as in KV Get")
}

// TestLeaderLastMsgBoundsAContextWithoutDeadline gives up after the
// JetStream context's default timeout when nothing answers, as nats.go's
// own API calls do, instead of waiting on the caller's context forever.
func TestLeaderLastMsgBoundsAContextWithoutDeadline(t *testing.T) {
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	// A responder that never replies, so the request neither fails fast
	// with no responders nor completes.
	_, err = nc.Subscribe("silent.API.>", func(*nats.Msg) {})
	require.NoError(t, err)
	js, err := jsapi.NewWithAPIPrefix(nc, "silent.API", jsapi.WithDefaultTimeout(200*time.Millisecond))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := leaderLastMsg(context.Background(), js, "KV_x", "$KV.x.k")
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("read without a deadline did not time out")
	}
}

// TestAPIPrefixMatchesNatsGo compares the prefix the leader read uses with
// the subject nats.go itself sends, for each way a context is built.
func TestAPIPrefixMatchesNatsGo(t *testing.T) {
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	for name, build := range map[string]func(...jsapi.JetStreamOpt) (jsapi.JetStream, error){
		"default": func(o ...jsapi.JetStreamOpt) (jsapi.JetStream, error) {
			return jsapi.New(nc, o...)
		},
		"domain": func(o ...jsapi.JetStreamOpt) (jsapi.JetStream, error) {
			return jsapi.NewWithDomain(nc, "hub", o...)
		},
		"prefix": func(o ...jsapi.JetStreamOpt) (jsapi.JetStream, error) {
			return jsapi.NewWithAPIPrefix(nc, "pre.API", o...)
		},
		"prefix dotted": func(o ...jsapi.JetStreamOpt) (jsapi.JetStream, error) {
			return jsapi.NewWithAPIPrefix(nc, "pre.API.", o...)
		},
	} {
		var sent string
		js, err := build(jsapi.WithClientTrace(&jsapi.ClientTrace{RequestSent: func(subj string, _ []byte) { sent = subj }}))
		require.NoError(t, err, name)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = js.AccountInfo(ctx)
		cancel()
		require.Equal(t, sent, apiPrefix(js.Options())+"INFO", name)
	}
}

func startJetStream(t *testing.T) jsapi.JetStream {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jsapi.New(nc)
	require.NoError(t, err)
	return js
}

// TestLeaderStateKeysStopsAtTheBound ignores records written after the bound
// captured before the pass. A later delete still removes a key: with
// history 1 it also removes the key's earlier record.
func TestLeaderStateKeysStopsAtTheBound(t *testing.T) {
	js := startJetStream(t)
	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jsapi.KeyValueConfig{Bucket: "b", History: 1})
	require.NoError(t, err)
	// The bound falls on a record outside the prefix, so the next match is
	// past it.
	for _, k := range []string{"p.a", "p.b", "other.a"} {
		_, err := kv.Put(ctx, k, []byte("v"))
		require.NoError(t, err)
	}
	st, err := js.Stream(ctx, "KV_b")
	require.NoError(t, err)
	bound := st.CachedInfo().State.LastSeq
	_, err = kv.Put(ctx, "p.c", []byte("v"))
	require.NoError(t, err)

	keys, err := leaderStateKeys(ctx, js, "b", "p.", bound)
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"p.a": {}, "p.b": {}}, keys)

	// A bound on the last match costs one read per record, with no read
	// past it.
	counted := &cancelOnRead{JetStream: js, cancel: func() {}}
	keys, err = leaderStateKeys(ctx, counted, "b", "p.", bound+1)
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"p.a": {}, "p.b": {}, "p.c": {}}, keys)
	assert.Equal(t, 3, counted.reads)

	require.NoError(t, kv.Delete(ctx, "p.a"))
	keys, err = leaderStateKeys(ctx, js, "b", "p.", bound)
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"p.b": {}}, keys)

	keys, err = leaderStateKeys(ctx, js, "b", "p.", 0)
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// cancelOnRead counts leader reads and cancels its context as the nth
// starts; zero never cancels.
type cancelOnRead struct {
	jsapi.JetStream

	n, reads int
	cancel   context.CancelFunc
}

func (c *cancelOnRead) Options() jsapi.JetStreamOptions {
	if c.reads++; c.reads == c.n {
		c.cancel()
	}
	return c.JetStream.Options()
}

// TestLeaderStateKeysStopsWhenCancelled returns the context's error from a
// pass cancelled between records rather than a partial key set.
func TestLeaderStateKeysStopsWhenCancelled(t *testing.T) {
	js := startJetStream(t)
	kv, err := js.CreateKeyValue(context.Background(), jsapi.KeyValueConfig{Bucket: "c", History: 1})
	require.NoError(t, err)
	for _, k := range []string{"p.a", "p.b", "p.c"} {
		_, err := kv.Put(context.Background(), k, []byte("v"))
		require.NoError(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cj := &cancelOnRead{JetStream: js, n: 2, cancel: cancel}
	keys, err := leaderStateKeys(ctx, cj, "c", "p.", 3)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, keys)
	assert.Equal(t, 2, cj.reads)
}
