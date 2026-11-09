package kvstore_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func durableConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Retention:         jetstream.LimitsPolicy,
		Storage:           jetstream.FileStorage,
		MaxMsgsPerSubject: 1,
		Discard:           jetstream.DiscardNew,
		Replicas:          1,
	}
}

func TestCheckDurable_RefusesEachViolation(t *testing.T) {
	require.NoError(t, kvstore.CheckDurable(durableConfig(), 1))

	cases := map[string]func(*jetstream.StreamConfig){
		"retention":      func(c *jetstream.StreamConfig) { c.Retention = jetstream.WorkQueuePolicy },
		"max age":        func(c *jetstream.StreamConfig) { c.MaxAge = time.Hour },
		"message ttl":    func(c *jetstream.StreamConfig) { c.AllowMsgTTL = true },
		"delete markers": func(c *jetstream.StreamConfig) { c.SubjectDeleteMarkerTTL = time.Minute },
		"memory storage": func(c *jetstream.StreamConfig) { c.Storage = jetstream.MemoryStorage },
		"async persist":  func(c *jetstream.StreamConfig) { c.PersistMode = jetstream.AsyncPersistMode },
		"max bytes":      func(c *jetstream.StreamConfig) { c.MaxBytes = 1 << 20 },
		"max msgs":       func(c *jetstream.StreamConfig) { c.MaxMsgs = 10 },
		"max msg size":   func(c *jetstream.StreamConfig) { c.MaxMsgSize = 1024 },
		"unbounded keys": func(c *jetstream.StreamConfig) { c.MaxMsgsPerSubject = -1 },
		"discard old":    func(c *jetstream.StreamConfig) { c.Discard = jetstream.DiscardOld },
		"per subject":    func(c *jetstream.StreamConfig) { c.DiscardNewPerSubject = true },
		"no ack":         func(c *jetstream.StreamConfig) { c.NoAck = true },
		"sealed":         func(c *jetstream.StreamConfig) { c.Sealed = true },
		"mirror":         func(c *jetstream.StreamConfig) { c.Mirror = &jetstream.StreamSource{Name: "x"} },
		"sources":        func(c *jetstream.StreamConfig) { c.Sources = []*jetstream.StreamSource{{Name: "x"}} },
		"replicas":       func(c *jetstream.StreamConfig) { c.Replicas = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			sc := durableConfig()
			mutate(&sc)
			want := 1
			if name == "replicas" {
				want = 3
			}
			assert.Error(t, kvstore.CheckDurable(sc, want))
		})
	}
}

func TestBindDurable_AbsentBucketIsNotCreated(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := t.Context()

	_, err := kvstore.BindDurable(ctx, js, "absent")
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound)

	_, err = js.KeyValue(ctx, "absent")
	assert.ErrorIs(t, err, jetstream.ErrBucketNotFound)
}

func TestBindDurable_RefusesTTLBucketAcceptsDurable(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := t.Context()

	_, err := kvstore.GetOrCreateBucketWithOptions(ctx, js, kvstore.BucketOptions{Name: "ok", History: 1})
	require.NoError(t, err)
	_, err = kvstore.BindDurable(ctx, js, "ok")
	require.NoError(t, err)

	_, err = kvstore.GetOrCreateBucketWithOptions(ctx, js, kvstore.BucketOptions{Name: "ttl", History: 1, TTL: time.Hour})
	require.NoError(t, err)
	_, err = kvstore.BindDurable(ctx, js, "ttl")
	require.ErrorContains(t, err, "not durable")
}
