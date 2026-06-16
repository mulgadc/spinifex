//go:build e2e

package multinode

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// jetStreamReplicaBuckets are the daemon-owned KV buckets created at replicas=1
// during initJetStream and bumped to the cluster size by upgradeJetStreamReplicas.
// A regression there leaves them stuck at 1, silently losing EC2/cluster state
// on a single node loss — exactly what this guards (the upgrade path is 0% unit).
var jetStreamReplicaBuckets = []string{
	daemon.InstanceStateBucket,
	daemon.ClusterStateBucket,
	daemon.TerminatedInstanceBucket,
}

// runJetStreamReplicas asserts every daemon KV bucket's configured replica factor
// matches the cluster node count, proving upgradeJetStreamReplicas ran end-to-end
// (the assembled NATS path no unit test reaches). Read-only over NATS.
func runJetStreamReplicas(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — JetStream KV Replica Factor")

	want := len(fix.Cluster.Nodes)
	if want < 2 {
		t.Skipf("cluster has %d node(s); replica upgrade is a no-op below 2", want)
	}
	harness.Detail(t, "cluster_nodes", want, "buckets", strings.Join(jetStreamReplicaBuckets, ","))

	harness.Step(t, "introspect KV stream replica factors")
	got := harness.KVReplicaFactors(t, fix.Env, jetStreamReplicaBuckets...)
	for _, b := range jetStreamReplicaBuckets {
		harness.Detail(t, "bucket", b, "replicas", got[b])
		require.Equalf(t, want, got[b],
			"KV bucket %s replicas=%d want=%d — upgradeJetStreamReplicas did not match cluster size; a single node loss would lose this bucket",
			b, got[b], want)
	}
}
