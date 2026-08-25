package kvlease_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvlease"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBucket = "kvlease-test"
	testKey    = "leader"
)

// newTestLease builds a lease over a short-lived bucket so a test can outlast
// the TTL in a second rather than a minute. Bucket is built after mut so a test
// that lengthens the TTL gets a bucket that matches it.
func newTestLease(t *testing.T, nc *nats.Conn, holder string, mut func(*kvlease.Config)) *kvlease.Lease {
	t.Helper()
	cfg := kvlease.Config{
		Name:   "test",
		Key:    testKey,
		Holder: holder,
		TTL:    time.Second,
		Renew:  250 * time.Millisecond,
		Retry:  100 * time.Millisecond,
	}
	if mut != nil {
		mut(&cfg)
	}
	cfg.Bucket = kvlease.NATSBucket(nc, testBucket, cfg.TTL)
	l, err := kvlease.New(cfg)
	require.NoError(t, err)
	return l
}

func testKV(t *testing.T, nc *nats.Conn, ttl time.Duration) jetstream.KeyValue {
	t.Helper()
	kv, err := kvlease.NATSBucket(nc, testBucket, ttl)(t.Context())
	require.NoError(t, err)
	return kv
}

// isOpen reports whether a Lost channel has not yet fired, without blocking.
func isOpen(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

func TestTryAcquire_SecondHolderLoses(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	a := newTestLease(t, nc, "node-a", nil)
	b := newTestLease(t, nc, "node-b", nil)

	require.True(t, a.TryAcquire(t.Context()), "first acquire must win and create the bucket")
	assert.False(t, b.TryAcquire(t.Context()), "second acquire must lose while the lease is held")

	a.Release(t.Context())
	assert.True(t, b.TryAcquire(t.Context()), "acquire after release must win")
	b.Release(t.Context())
}

// A restarted process finds its own key still present. Refusing it would leave the subsystem leaderless for the rest of
// the TTL for no reason.
func TestTryAcquire_AdoptsOwnKeyAfterRestart(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := testKV(t, nc, time.Second)

	rev, err := kv.Create(t.Context(), testKey, []byte("node-a"))
	require.NoError(t, err)

	restarted := newTestLease(t, nc, "node-a", nil)
	require.True(t, restarted.TryAcquire(t.Context()), "a holder must adopt the key it left behind")

	entry, err := kv.Get(t.Context(), testKey)
	require.NoError(t, err)
	assert.Greater(t, entry.Revision(), rev, "adopting must refresh the key, resetting its TTL")
	restarted.Release(t.Context())
}

// A pass can outlive the TTL on its own, so the key must be renewed while the pass runs. Without this a peer elects
// itself mid-pass and both act at once.
func TestRenew_KeySurvivesBeyondTTL(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	a := newTestLease(t, nc, "node-a", nil)
	require.True(t, a.TryAcquire(t.Context()))
	defer a.Release(t.Context())

	time.Sleep(2500 * time.Millisecond)

	b := newTestLease(t, nc, "node-b", nil)
	assert.False(t, b.TryAcquire(t.Context()), "lease expired mid-pass: a peer can now act concurrently")
	assert.True(t, a.Held())
}

// If the key was lost anyway, the stale holder's release must not delete the successor's key -
// that hand the lease to a third node silently.
func TestRelease_DoesNotDeleteSuccessorKey(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	a := newTestLease(t, nc, "node-a", func(c *kvlease.Config) {
		c.TTL, c.Renew = time.Minute, 20*time.Second // no renewal during the test
	})
	require.True(t, a.TryAcquire(t.Context()))

	kv := testKV(t, nc, time.Minute)
	_, err := kv.Put(t.Context(), testKey, []byte("node-b"))
	require.NoError(t, err)

	a.Release(t.Context())

	entry, err := kv.Get(t.Context(), testKey)
	require.NoError(t, err, "stale release deleted the successor's key")
	assert.Equal(t, "node-b", string(entry.Value()))
}

// Shutdown cancels the acquiring context first. A release bound to it would no-op and park the lease for the full TTL.
func TestRelease_SurvivesCancelledContext(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	ctx, cancel := context.WithCancel(t.Context())
	a := newTestLease(t, nc, "node-a", nil)
	require.True(t, a.TryAcquire(ctx))

	cancel()
	a.Release(ctx)

	b := newTestLease(t, nc, "node-b", nil)
	assert.True(t, b.TryAcquire(t.Context()), "lease must be free immediately, not at the TTL")
	b.Release(t.Context())
}

func TestEdges_FireOncePerTransition(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	var gained, lost atomic.Int32
	a := newTestLease(t, nc, "node-a", func(c *kvlease.Config) {
		c.OnGained = func(context.Context) error { gained.Add(1); return nil }
		c.OnLost = func() { lost.Add(1) }
	})

	require.True(t, a.TryAcquire(t.Context()))
	require.True(t, a.TryAcquire(t.Context()), "a holder re-attempting must stay held")
	assert.EqualValues(t, 1, gained.Load(), "OnGained fired twice for one election")
	assert.EqualValues(t, 0, lost.Load())

	a.Release(t.Context())
	assert.EqualValues(t, 1, lost.Load())
	a.Release(t.Context())
	assert.EqualValues(t, 1, lost.Load(), "a second release must not re-fire OnLost")
}

func TestRenew_LossFiresOnLost(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	lost := make(chan struct{}, 4)
	a := newTestLease(t, nc, "node-a", func(c *kvlease.Config) {
		c.OnLost = func() { lost <- struct{}{} }
	})
	require.True(t, a.TryAcquire(t.Context()))

	// Stand in for the key expiring and a peer claiming it.
	kv := testKV(t, nc, time.Second)
	require.NoError(t, kv.Delete(t.Context(), testKey))
	_, err := kv.Create(t.Context(), testKey, []byte("node-b"))
	require.NoError(t, err)

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal lost the CAS but OnLost never fired")
	}
	assert.False(t, a.Held())
}

// A leader that cannot set up its own work is worse than no leader.
func TestTryAcquire_OnGainedFailureStandsDown(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	var lost atomic.Int32
	a := newTestLease(t, nc, "node-a", func(c *kvlease.Config) {
		c.OnGained = func(context.Context) error { return errors.New("subscribe failed") }
		c.OnLost = func() { lost.Add(1) }
	})

	assert.False(t, a.TryAcquire(t.Context()), "a holder that cannot set up its work must not report leadership")
	assert.False(t, a.Held())
	assert.EqualValues(t, 1, lost.Load(), "OnLost must fire so the caller tears down partial statea")

	b := newTestLease(t, nc, "node-b", nil)
	assert.True(t, b.TryAcquire(t.Context()), "standing down must free the key immediately")
	b.Release(t.Context())
}

func TestNew_RejectUnworkableConfig(t *testing.T) {
	base := func() kvlease.Config {
		return kvlease.Config{
			Bucket: func(context.Context) (jetstream.KeyValue, error) { return nil, nil },
			Key:    testKey,
			Holder: "node-a",
			TTL:    time.Minute,
			Renew:  20 * time.Second,
		}
	}
	_, err := kvlease.New(base())
	require.NoError(t, err)

	cases := map[string]func(*kvlease.Config){
		"no bucket":           func(c *kvlease.Config) { c.Bucket = nil },
		"no key":              func(c *kvlease.Config) { c.Key = "" },
		"no holder":           func(c *kvlease.Config) { c.Holder = "" },
		"no ttl":              func(c *kvlease.Config) { c.TTL = 0 },
		"renew equals ttl":    func(c *kvlease.Config) { c.Renew = c.TTL },
		"renew over half ttl": func(c *kvlease.Config) { c.Renew = 31 * time.Second },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mut(&cfg)
			_, err := kvlease.New(cfg)
			assert.Error(t, err)
		})
	}
}

// Session mode: a loser must take over on its retry tick rather than wait out the TTL, which is what makes a rolling restart a sub-second gap.
func TestRun_LoserTakesOverAfterLeaderStops(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	aCtx, stopA := context.WithCancel(t.Context())
	a := newTestLease(t, nc, "node-a", nil)
	go a.Run(aCtx)
	require.Eventually(t, a.Held, 2*time.Second, 20*time.Millisecond)

	b := newTestLease(t, nc, "node-b", nil)
	go b.Run(t.Context())
	time.Sleep(300 * time.Millisecond)
	require.False(t, b.Held(), "two holders at once")

	stopA()
	require.Eventually(t, b.Held, 2*time.Second, 20*time.Millisecond, "loser must take over on its retry tick, not at the TTL")
}

// Work gated on Lose must run while the lease is held. A channel closed at construction or on acquire would abort every pass before it started.
func TestLost_OpenWhileHeld(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	a := newTestLease(t, nc, "node-a", nil)

	assert.True(t, isOpen(a.Lost()), "an unclaimed lease has lost nothing")

	require.True(t, a.TryAcquire(t.Context()))
	assert.True(t, isOpen(a.Lost()))

	// Well past two renewal intervals: a renewing holder must stay held.
	time.Sleep(600 * time.Millisecond)
	assert.True(t, isOpen(a.Lost()), "renewal succeeded but lost fired anyway")

	a.Release(t.Context())
}

// The signal callers select on to abandon work. Polling Held cannot express "stop what you are doing now", which is what a lost lease requires.
func TestLost_ClosesOnRenewalLoss(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	a := newTestLease(t, nc, "node-a", nil)
	require.True(t, a.TryAcquire(t.Context()))
	lost := a.Lost()

	// Stand in for the key expiring and a peer claiming it.
	kv := testKV(t, nc, time.Second)
	require.NoError(t, kv.Delete(t.Context(), testKey))
	_, err := kv.Create(t.Context(), testKey, []byte("node-b"))
	require.NoError(t, err)

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal lost the CAS but Lost never closed")
	}
	assert.False(t, a.Held())
}

func TestLost_ClosesOnRelease(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	a := newTestLease(t, nc, "node-a", nil)
	require.True(t, a.TryAcquire(t.Context()))
	lost := a.Lost()

	a.Release(t.Context())
	assert.False(t, isOpen(lost), "release must close Lost so gated work stops")

	// A second release must not close an already-closed channel.
	assert.NotPanics(t, func() { a.Release(t.Context()) })
}

// The channel is per-acquisition. Handing a re-elected holder the closed channel from its previous term would abort its new pass immediately.
func TestLost_FreshChannelAfterReacquire(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	a := newTestLease(t, nc, "node-a", nil)

	require.True(t, a.TryAcquire(t.Context()))
	first := a.Lost()
	a.Release(t.Context())
	require.False(t, isOpen(first))

	require.True(t, a.TryAcquire(t.Context()))
	second := a.Lost()
	assert.True(t, isOpen(second), "a re-elected holder got its old term's closed channel")
	assert.NotEqual(t, first, second, "Lost must be replaced on acquire, not reused")

	a.Release(t.Context())
}

func TestAttrs_AppearOnLeaseOwnedLogLines(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	lease := newTestLease(t, nc, "node-a", func(cfg *kvlease.Config) {
		cfg.Attrs = []any{"bucket", "quota-reconcile"}
	})
	require.True(t, lease.TryAcquire(t.Context()))
	lease.Release(t.Context())

	require.Contains(t, buf.String(), `"bucket":"quota-reconcile"`)
}

// A node that never won the lease still unwinds its Run loop through Release.
// That is not a lost key, so it must not raise the warning that says one.
func TestRelease_WithoutAcquireIsSilent(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	holder := newTestLease(t, nc, "node-a", nil)
	require.True(t, holder.TryAcquire(t.Context()))
	t.Cleanup(func() { holder.Release(t.Context()) })

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	loser := newTestLease(t, nc, "node-b", nil)
	require.False(t, loser.TryAcquire(t.Context()))
	loser.Release(t.Context())

	assert.NotContains(t, buf.String(), "already lost before release")
}

// The warning still has to fire where it means something: renewal lost the key,
// so a delete here would drop the successor's lease rather than.
func TestRelease_AfterRenewalLossWarns(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	lost := make(chan struct{}, 4)
	a := newTestLease(t, nc, "node-a", func(c *kvlease.Config) {
		c.OnLost = func() { lost <- struct{}{} }
	})
	require.True(t, a.TryAcquire(t.Context()))

	kv := testKV(t, nc, time.Second)
	require.NoError(t, kv.Delete(t.Context(), testKey))
	_, err := kv.Create(t.Context(), testKey, []byte("node-b"))
	require.NoError(t, err)

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal lost the CAS but OnLost never fired")
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a.Release(t.Context())
	assert.Contains(t, buf.String(), "already lost before release")
}
