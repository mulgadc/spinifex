package northstar

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
)

func TestNewRejectsWrongConfigType(t *testing.T) {
	_, err := New(struct{}{})
	require.Error(t, err)
}

func TestNewAcceptsConfig(t *testing.T) {
	cfg := &Config{
		ConfigPath: "/etc/spinifex/northstar/northstar.toml",
		NatsHost:   "127.0.0.1:4222",
		NatsToken:  "token",
		NatsCACert: "/etc/spinifex/ca.pem",
	}
	svc, err := New(cfg)
	require.NoError(t, err)
	require.NotNil(t, svc)
	assert.Equal(t, cfg, svc.Config)
}

func TestStartRequiresConfigPath(t *testing.T) {
	svc, err := New(&Config{})
	require.NoError(t, err)
	_, err = svc.Start()
	require.Error(t, err)
}

type fakeReloader struct {
	etag  string
	err   error
	zones chan string
}

func (f *fakeReloader) ReloadZoneVersion(zone string) (string, error) {
	f.zones <- zone
	return f.etag, f.err
}

func subscribeHandler(t *testing.T, nc *nats.Conn, r zoneReloader) {
	t.Helper()
	sub, err := nc.Subscribe(dns.SubjectZoneReload, func(msg *nats.Msg) { handleReload(r, "node2", msg) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestHandleReloadAcknowledgesServedVersion(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	r := &fakeReloader{etag: "abc123", zones: make(chan string, 1)}
	subscribeHandler(t, nc, r)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acks, err := dns.RequestZoneReload(ctx, nc, "example.com", 1)
	require.NoError(t, err)
	require.Len(t, acks, 1)
	assert.Equal(t, dns.ZoneReloadAck{Zone: "example.com", Node: "node2", ETag: "abc123"}, acks[0])
	assert.Equal(t, "example.com", <-r.zones)
}

func TestHandleReloadAcknowledgesFailure(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	r := &fakeReloader{etag: "stale", err: errors.New("fetch zone: timeout"), zones: make(chan string, 1)}
	subscribeHandler(t, nc, r)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acks, err := dns.RequestZoneReload(ctx, nc, "example.com", 1)
	require.NoError(t, err)
	require.Len(t, acks, 1)
	assert.Equal(t, "fetch zone: timeout", acks[0].Error)
	assert.Empty(t, acks[0].ETag, "a failed reload must not report a version")
}

func TestHandleReloadWithoutReplyStillReloads(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	r := &fakeReloader{zones: make(chan string, 1)}
	subscribeHandler(t, nc, r)

	body, err := json.Marshal(dns.ZoneReload{Zone: "example.com"})
	require.NoError(t, err)
	require.NoError(t, nc.Publish(dns.SubjectZoneReload, body))
	select {
	case zone := <-r.zones:
		assert.Equal(t, "example.com", zone)
	case <-time.After(5 * time.Second):
		t.Fatal("a fire-and-forget reload was not applied")
	}
}

func TestHandleReloadIgnoresEventWithoutZone(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	r := &fakeReloader{zones: make(chan string, 1)}
	subscribeHandler(t, nc, r)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	acks, err := dns.RequestZoneReload(ctx, nc, "", 1)
	require.NoError(t, err)
	assert.Empty(t, acks)
	assert.Empty(t, r.zones)
}
