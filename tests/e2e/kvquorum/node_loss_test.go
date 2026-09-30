//go:build e2e

package kvquorum

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	natssvc "github.com/mulgadc/spinifex/spinifex/services/nats"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runBucketsSurviveANodeLoss produces the fault rather than inspecting for it.
//
// The node taken away is the one leading the most buckets, which is the worst
// one to lose and the one a test that picked arbitrarily would usually miss.
// On a correctly replicated cluster the survivors elect new leaders and keep
// answering; on a cluster of single-replica buckets, every bucket that lived
// on that node answers "no responders" from every node at once, which is the
// outage this whole change exists to remove.
func runBucketsSurviveANodeLoss(t *testing.T, fix *Fixture) {
	harness.Phase(t, "KV quorum — the cluster's state survives losing a node")

	nodes := len(fix.Cluster.Nodes)
	want := harness.WantKVReplicas(nodes)

	harness.Step(t, "audit the cluster before anything is broken")
	before := harness.KVReplication(t, fix.Env, nodes)
	require.NotEmpty(t, before, "no KV buckets to lose")
	harness.RequireKVQuorum(t, fix.Env, nodes)

	victim, led := busiestLeader(fix.Cluster.Nodes, before)
	require.NotZerof(t, victim.Index,
		"no bucket named a leader this test can map to a node; peers were %s", peerNames(before))
	harness.Detail(t, "victim", victim.Name, "buckets_led", led, "buckets_total", len(before))

	survivors := fix.Cluster.Peers(victim)

	harness.Step(t, "take %s away", victim.Name)
	// Registered before the stop, so a panic or a cancelled run still brings
	// the node back rather than leaving the cluster a node short.
	t.Cleanup(func() {
		harness.StartNode(t, victim)
		harness.WaitNodeServiceReady(t, victim, harness.WithTimeout(rejoinBudget))
	})
	harness.StopNode(t, victim)

	harness.Step(t, "every survivor must still answer for every bucket")
	for _, node := range survivors {
		requireNodeSeesEveryBucket(t, node, before, want)
	}

	harness.Step(t, "bring %s back", victim.Name)
	harness.StartNode(t, victim)
	harness.WaitNodeServiceReady(t, victim, harness.WithTimeout(rejoinBudget))
	fix.Cluster.WaitNATSPeers(t, nodes-1, harness.WithTimeout(rejoinBudget))

	harness.Step(t, "the returning node must carry its share again")
	for _, node := range fix.Cluster.Nodes {
		requireNodeSeesEveryBucket(t, node, before, want)
	}

	// Configured replicas must be unchanged. A cluster that repaired itself by
	// lowering a count would pass every assertion above and have quietly traded
	// the durability this suite is about for a green run.
	harness.Step(t, "no bucket lost replicas on the way through")
	after := harness.KVReplication(t, fix.Env, nodes)
	requireNoBucketLostReplicas(t, before, after)
	harness.RequireKVQuorum(t, fix.Env, nodes)
}

// requireNodeSeesEveryBucket polls a node until its own audit names every
// bucket the cluster had before the outage, with none under-replicated.
//
// Polling rather than asserting once: a raft group whose leader has gone has to
// elect a new one, and that is the interval this test is required to tolerate.
// What it is not required to tolerate is a bucket that never comes back.
func requireNodeSeesEveryBucket(t *testing.T, node harness.Node, before []kvutil.BucketReport, want int) {
	t.Helper()
	wantBuckets := bucketNames(before)

	harness.EventuallyErr(t, func() error {
		reports, err := nodeReplicaReport(t, node)
		if err != nil {
			return fmt.Errorf("%s: %w", node.Name, err)
		}
		if missing := missingFrom(wantBuckets, bucketNames(reports)); len(missing) > 0 {
			return fmt.Errorf("%s cannot read %d bucket(s): %s",
				node.Name, len(missing), strings.Join(missing, ", "))
		}
		if under := underReplicated(reports, want); len(under) > 0 {
			return fmt.Errorf("%s reports %d under-replicated bucket(s): %s",
				node.Name, len(under), strings.Join(under, ", "))
		}
		return nil
	}, quorumRecoveryBudget, pollInterval)

	harness.Detail(t, "node", node.Name, "buckets_readable", len(wantBuckets))
}

// requireNoBucketLostReplicas fails if any bucket came back with fewer replicas
// than it had, which is a silent downgrade rather than a repair.
func requireNoBucketLostReplicas(t *testing.T, before, after []kvutil.BucketReport) {
	t.Helper()
	was := make(map[string]int, len(before))
	for _, r := range before {
		was[r.Bucket] = r.Replicas
	}
	for _, r := range after {
		if prev, ok := was[r.Bucket]; ok && r.Replicas < prev {
			t.Fatalf("KV bucket %s went from %d replicas to %d across the outage", r.Bucket, prev, r.Replicas)
		}
	}
}

// busiestLeader returns the node leading the most buckets, and how many.
// Leadership is where the loss hurts most: a follower going away costs a raft
// group a peer, a leader going away costs it an election.
//
// The leader is read from the field that holds it rather than guessed at the head
// of the peer list. It is a NATS server name, which is the node name behind a
// prefix rather than the node name itself, so it goes through the helper that
// owns that prefix.
func busiestLeader(nodes []harness.Node, reports []kvutil.BucketReport) (harness.Node, int) {
	led := make(map[string]int, len(nodes))
	for _, r := range reports {
		if node, ok := natssvc.NodeFromServerName(r.Leader); ok {
			led[node]++
		}
	}

	var best harness.Node
	bestN := -1
	for _, n := range nodes {
		if c := led[n.Name]; c > bestN {
			best, bestN = n, c
		}
	}
	if bestN <= 0 {
		return harness.Node{}, 0
	}
	return best, bestN
}

// bucketNames returns the sorted bucket names in a report set.
func bucketNames(reports []kvutil.BucketReport) []string {
	out := make([]string, 0, len(reports))
	for _, r := range reports {
		out = append(out, r.Bucket)
	}
	sort.Strings(out)
	return out
}

// missingFrom returns every name in want that is absent from got.
func missingFrom(want, got []string) []string {
	have := make(map[string]struct{}, len(got))
	for _, g := range got {
		have[g] = struct{}{}
	}
	var out []string
	for _, w := range want {
		if _, ok := have[w]; !ok {
			out = append(out, w)
		}
	}
	return out
}

// peerNames is diagnostic only: it says what the audit did report when no
// leader could be mapped to a node, which is otherwise an opaque failure.
func peerNames(reports []kvutil.BucketReport) string {
	seen := map[string]struct{}{}
	for _, r := range reports {
		for _, p := range r.Peers {
			seen[p] = struct{}{}
		}
	}
	out := slices.Sorted(maps.Keys(seen))
	return strings.Join(out, ",")
}
