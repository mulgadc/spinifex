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
