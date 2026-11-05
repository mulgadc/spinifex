//go:build e2e

package kvquorum

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// replicasCmd is the operator surface under test. No --config: on a node the
// command finds /etc/spinifex/spinifex.toml itself, which is how anyone
// gating a deployment on it would actually run it.
const replicasCmd = "sudo spx admin kv replicas --json"

// runBucketsAreReplicated sweeps every KV bucket in the cluster rather than a
// named list. A replica count is set where a bucket is created, and there is a
// bucket-creating call in most services, so a named list only ever covers the
// ones somebody remembered.
func runBucketsAreReplicated(t *testing.T, fix *Fixture) {
	harness.Phase(t, "KV quorum — every bucket is replicated across the cluster")

	nodes := len(fix.Cluster.Nodes)
	want := harness.WantKVReplicas(nodes)
	harness.Detail(t, "cluster_nodes", nodes, "want_replicas", want)

	harness.Step(t, "audit every KV bucket")
	reports := harness.KVReplication(t, fix.Env, nodes)
	require.NotEmpty(t, reports, "the cluster reports no KV buckets at all, so nothing was checked")

	for _, r := range reports {
		harness.Detail(t, "bucket", r.Bucket, "replicas", r.Replicas, "held_by", strings.Join(r.Peers, ","))
	}
	harness.RequireKVQuorum(t, fix.Env, nodes)

	// The sync bucket is named as well as swept: every leader election runs in
	// it, so if it loses quorum, no reconciler in the cluster can lead.
	harness.Step(t, "confirm the sync bucket is replicated and the old lease buckets are gone")
	sync, found := findBucket(reports, syncBucket)
	require.True(t, found, "no %s bucket was found; either no election has run or the audit missed it", syncBucket)
	require.Equalf(t, want, sync.Replicas, "%s holds %d replicas, want %d", syncBucket, sync.Replicas, want)
	require.Empty(t, presentBuckets(reports, obsoleteLeaseBuckets),
		"the daemon deletes the per-caller lease buckets at start, so none may survive")
}

// syncBucket is where every kvsync claim and lease lives.
const syncBucket = "spinifex-sync"

// obsoleteLeaseBuckets are the per-caller lease buckets the sync bucket replaced.
var obsoleteLeaseBuckets = []string{
	"spinifex-vpcd-reconcile",
	"spinifex-dns-reconcile",
	"spinifex-route53-realize",
	"spinifex-route53-verify",
	"spinifex-quota-reconcile",
	"spinifex-rds-leader",
	"spinifex-ecs-leader",
	"bedrock-leader",
	"spinifex-eks-leader",
}

func findBucket(reports []kvstore.BucketReport, name string) (kvstore.BucketReport, bool) {
	for _, r := range reports {
		if r.Bucket == name {
			return r, true
		}
	}
	return kvstore.BucketReport{}, false
}

// presentBuckets returns those of names that appear in reports, sorted.
func presentBuckets(reports []kvstore.BucketReport, names []string) []string {
	var out []string
	for _, name := range names {
		if _, ok := findBucket(reports, name); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// runReplicasCommand proves the operator surface on every node, not one. The
// command reads cluster-global raft metadata, so every node must give the same
// answer — a node that disagrees is reporting on a cluster it is not fully part
// of, which is worth knowing before anything is gated on its exit status.
func runReplicasCommand(t *testing.T, fix *Fixture) {
	harness.Phase(t, "KV quorum — spx admin kv replicas agrees on every node")

	want := harness.WantKVReplicas(len(fix.Cluster.Nodes))

	counts := make(map[string]int, len(fix.Cluster.Nodes))
	for _, node := range fix.Cluster.Nodes {
		harness.Step(t, "run the replica audit on %s", node.Name)
		reports, err := nodeReplicaReport(t, node)
		require.NoErrorf(t, err,
			"%s could not audit its own KV buckets, so nothing can be gated on its exit status", node.Name)

		under := underReplicated(reports, want)
		harness.Detail(t, "node", node.Name, "buckets", len(reports), "under_replicated", len(under))
		require.Emptyf(t, under,
			"%s reports %d under-replicated bucket(s): %s", node.Name, len(under), strings.Join(under, ", "))
		counts[node.Name] = len(reports)
	}

	harness.Step(t, "confirm every node sees the same set of buckets")
	var first string
	for _, node := range fix.Cluster.Nodes {
		if first == "" {
			first = node.Name
			continue
		}
		require.Equalf(t, counts[first], counts[node.Name],
			"%s sees %d KV buckets and %s sees %d; the two are not looking at the same cluster",
			first, counts[first], node.Name, counts[node.Name])
	}
}

// nodeReplicaReport runs the replica audit on one node and decodes it. A
// non-zero exit is returned as an error rather than fatalled, because callers
// polling a recovering cluster need to retry it.
func nodeReplicaReport(t *testing.T, node harness.Node) ([]kvstore.BucketReport, error) {
	t.Helper()
	ssh := harness.NewPeerSSH()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := ssh.Run(ctx, node.Addr, replicasCmd)
	if err != nil {
		// The command exits 1 when a bucket is under-replicated, and it still
		// prints its report, so the output is decoded either way — a report
		// naming the bucket is a better failure than "exit status 1".
		if len(out) == 0 {
			return nil, fmt.Errorf("%s: %w", node.Name, err)
		}
	}
	var reports []kvstore.BucketReport
	if jsonErr := json.Unmarshal(trimToJSON(out), &reports); jsonErr != nil {
		return nil, fmt.Errorf("%s: decode replica report (%q): %w", node.Name, string(out), jsonErr)
	}
	return reports, nil
}

// trimToJSON drops anything before the JSON document, since sudo and the login
// shell can both write to the same stream.
func trimToJSON(out []byte) []byte {
	if i := strings.IndexAny(string(out), "[{"); i > 0 {
		return out[i:]
	}
	return out
}

// underReplicated names every bucket below want replicas.
func underReplicated(reports []kvstore.BucketReport, want int) []string {
	var out []string
	for _, r := range reports {
		if r.Replicas < want {
			out = append(out, fmt.Sprintf("%s(%d/%d)", r.Bucket, r.Replicas, want))
		}
	}
	sort.Strings(out)
	return out
}
