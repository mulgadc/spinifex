package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/daemon"
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

// nodeTomlWithNATSHost writes a minimal single-node spinifex.toml pointing at
// the given NATS host, the shape probeJetStreamWrite's loadConfigAndConnect
// call needs to resolve node1's NATS endpoint.
func nodeTomlWithNATSHost(t *testing.T, natsHost string) string {
	t.Helper()
	return writeSpinifexToml(t, fmt.Sprintf(`
version = "1.0"
epoch = 1
node = "node1"

[nodes.node1]
node = "node1"
region = "us-east-1"
az = "us-east-1a"

[nodes.node1.nats]
host = %q
`, natsHost))
}

// A config file viper cannot parse must never read as a JetStream failure:
// the probe has not connected to anything yet.
func TestProbeJetStreamWrite_ConfigLoadFailureIsInconclusive(t *testing.T) {
	resetGlobalViper(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "spinifex.toml")
	require.NoError(t, os.WriteFile(path, []byte("not [[[ valid toml"), 0o600))
	viper.Set("config", path)

	err := probeJetStreamWrite(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, errProbeInconclusive, "config-load failure must be inconclusive")
}

// A NATS endpoint that refuses the connection must also read as inconclusive,
// not as JetStream refusing a write — this is the class of failure that
// turned a broken spx binary into a cluster-wide NATS restart.
func TestProbeJetStreamWrite_ConnectFailureIsInconclusive(t *testing.T) {
	resetGlobalViper(t)

	viper.Set("config", nodeTomlWithNATSHost(t, "nats://127.0.0.1:1"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := probeJetStreamWrite(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, errProbeInconclusive, "connect failure must be inconclusive")
}

// A reachable NATS with no cluster-state bucket yet (recovery, or a bucket
// that has not been created) must not read as a JetStream write failure.
func TestProbeJetStreamWrite_BucketOpenFailureIsInconclusive(t *testing.T) {
	resetGlobalViper(t)

	ns, _, _ := testutil.StartTestJetStream(t)
	viper.Set("config", nodeTomlWithNATSHost(t, ns.ClientURL()))

	err := probeJetStreamWrite(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, errProbeInconclusive, "missing bucket must be inconclusive")
}

// The positive end-to-end path: once the bucket exists and NATS accepts
// writes, the probe succeeds through every stage rather than just at the
// canaryRoundTrip unit level covered above.
func TestProbeJetStreamWrite_SucceedsAgainstRealClusterStateBucket(t *testing.T) {
	resetGlobalViper(t)

	ns, _, js := testutil.StartTestJetStream(t)
	_, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: daemon.ClusterStateBucket})
	require.NoError(t, err)

	viper.Set("config", nodeTomlWithNATSHost(t, ns.ClientURL()))

	require.NoError(t, probeJetStreamWrite(context.Background()))
}
