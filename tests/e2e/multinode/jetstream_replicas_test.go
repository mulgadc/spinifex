//go:build e2e

package multinode

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// jetStreamReplicaBuckets are the daemon-owned KV buckets. They are named
// explicitly as well as swept, so a run that somehow enumerates nothing still
// fails rather than passing on an empty set.
var jetStreamReplicaBuckets = []string{
	daemon.InstanceStateBucket,
	daemon.ClusterStateBucket,
	daemon.TerminatedInstanceBucket,
}

// runJetStreamReplicas asserts that every KV bucket in the cluster is
// replicated across every node, not just the three the daemon owns.
//
// The sweep is the assertion that matters. A replica count is set where a
// bucket is created, and there is a bucket-creating call in most services, so
// naming buckets here would only ever cover the ones somebody remembered. A
// bucket below the node count lives on fewer nodes than depend on it: losing
// one of them makes it unreadable from every node at once, which is how a
// single-node event becomes a cluster-wide outage.
func runJetStreamReplicas(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — JetStream KV Replica Factor")

	nodes := len(fix.Cluster.Nodes)
	if nodes < 2 {
		t.Skipf("cluster has %d node(s); replication across nodes is a no-op below 2", nodes)
	}
	want := harness.WantKVReplicas(nodes)
	harness.Detail(t, "cluster_nodes", nodes, "want_replicas", want)

	harness.Step(t, "introspect the daemon's own KV stream replica factors")
	got := harness.KVReplicaFactors(t, fix.Env, jetStreamReplicaBuckets...)
	for _, b := range jetStreamReplicaBuckets {
		harness.Detail(t, "bucket", b, "replicas", got[b])
		require.Equalf(t, want, got[b],
			"KV bucket %s replicas=%d want=%d — a single node loss would take this bucket down cluster-wide",
			b, got[b], want)
	}

	harness.Step(t, "sweep every KV bucket in the cluster")
	reports := harness.KVReplication(t, fix.Env, nodes)
	names := make([]string, 0, len(reports))
	for _, r := range reports {
		names = append(names, r.Bucket)
	}
	harness.Detail(t, "buckets", len(reports), "names", strings.Join(names, ","))
	harness.RequireKVQuorum(t, fix.Env, nodes)
}
