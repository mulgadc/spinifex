package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/testutil"
)

func newCanaryBucket(t *testing.T) (jetstream.KeyValue, jetstream.JetStream) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "canary-test"})
	require.NoError(t, err)
	return kv, js
}

func TestCanaryRoundTrip_HealthyBucketPasses(t *testing.T) {
	kv, _ := newCanaryBucket(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, canaryRoundTrip(ctx, kv))
}

// Each probe must write a distinct value, otherwise a bucket that silently
// stopped accepting writes would still read back the previous run's value and
// the probe would pass while JetStream was wedged.
func TestCanaryRoundTrip_WritesADistinctValueEachRun(t *testing.T) {
	kv, _ := newCanaryBucket(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, canaryRoundTrip(ctx, kv))
	first, err := kv.Get(ctx, canaryKey)
	require.NoError(t, err)

	require.NoError(t, canaryRoundTrip(ctx, kv))
	second, err := kv.Get(ctx, canaryKey)
	require.NoError(t, err)

	require.NotEqual(t, string(first.Value()), string(second.Value()))
	require.Greater(t, second.Revision(), first.Revision())
}

// A deleted bucket stands in for the unreachable-store case: the probe must
// report an error rather than treating a failed write as success.
func TestCanaryRoundTrip_FailsWhenBucketIsGone(t *testing.T) {
	kv, js := newCanaryBucket(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, js.DeleteKeyValue(ctx, "canary-test"))

	err := canaryRoundTrip(ctx, kv)
	require.Error(t, err)
	require.Contains(t, err.Error(), "canary")
}

// The canary key is namespaced so it cannot collide with a real control-plane
// key in the shared cluster-state bucket.
func TestCanaryKeyIsNamespaced(t *testing.T) {
	require.True(t, len(canaryKey) > 0 && canaryKey[0] == '_')
}
