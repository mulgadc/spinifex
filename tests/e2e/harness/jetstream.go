//go:build e2e

package harness

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// KVReplicaFactors connects to the cluster NATS and returns the configured
// replica factor for each named KV bucket, read from JetStream stream metadata
// (the "KV_" stream prefix is applied internally). The replica count lives in
// cluster-global Raft metadata, so any node's NATS reports the same value.
// Fatals if NATS is unreachable or a bucket's stream cannot be introspected.
func KVReplicaFactors(t *testing.T, env *Env, buckets ...string) map[string]int {
	t.Helper()
	host, token, ca := natsConn(t, env)
	nc, err := utils.ConnectNATS(host, token, ca)
	if err != nil {
		t.Fatalf("connect NATS %s: %v", host, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream context: %v", err)
	}

	out := make(map[string]int, len(buckets))
	for _, b := range buckets {
		stream, err := js.Stream(t.Context(), "KV_"+b)
		if err != nil {
			t.Fatalf("open stream for KV bucket %q: %v", b, err)
		}
		info, err := stream.Info(t.Context())
		if err != nil {
			t.Fatalf("stream info for KV bucket %q: %v", b, err)
		}
		out[b] = info.Config.Replicas
	}
	return out
}

// WantKVReplicas is how many nodes a KV bucket must be replicated across on a
// cluster of this size.
//
// It defers to the production rule rather than restating it. A test that carried
// its own copy would keep passing after the policy changed, which is the one
// thing this assertion must not do.
func WantKVReplicas(nodes int) int {
	return clustersize.ReplicasFor(nodes)
}

// DialClusterNATS opens a NATS connection to the cluster, using the caller's
// env for the address and credentials. The connection is closed on cleanup.
func DialClusterNATS(t *testing.T, env *Env) *nats.Conn {
	t.Helper()
	host, token, ca := natsConn(t, env)
	nc, err := utils.ConnectNATS(host, token, ca)
	if err != nil {
		t.Fatalf("connect NATS %s: %v", host, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// KVReplication reports every KV bucket in the cluster: its replica count, what
// the cluster's size entitles it to, and which nodes hold it.
//
// The node count is declared here because this process never loaded a cluster
// config, and the audit refuses to guess one rather than assuming a single
// replica. Declaring is write-once, so the reset makes a second call with the
// same count safe and a third with a different one a visible failure.
func KVReplication(t *testing.T, env *Env, nodes int) []kvutil.BucketReport {
	t.Helper()
	clustersize.ResetForTest()
	if err := clustersize.Declare(nodes); err != nil {
		t.Fatalf("declare cluster size %d: %v", nodes, err)
	}
	js, err := jetstream.New(DialClusterNATS(t, env))
	if err != nil {
		t.Fatalf("jetstream context: %v", err)
	}
	reports, err := kvutil.AuditBucketReplicas(t.Context(), js)
	if err != nil {
		t.Fatalf("audit KV bucket replicas: %v", err)
	}
	return reports
}

// RequireKVQuorum fails unless every KV bucket in the cluster is replicated
// across every node, and unless each one is actually held by that many distinct
// nodes rather than merely configured for it.
//
// A bucket below the node count lives on fewer nodes than depend on it, so
// losing one of them makes it unreadable and unwritable from every node at
// once — a cluster-wide outage caused by a single-node event. This runs in
// every multi-node suite rather than one of them, because a bucket created by
// any service on any cell is the one that can be wrong.
func RequireKVQuorum(t *testing.T, env *Env, nodes int) {
	t.Helper()
	want := WantKVReplicas(nodes)

	reports := KVReplication(t, env, nodes)
	if len(reports) == 0 {
		t.Fatal("the cluster reports no KV buckets at all, so nothing was checked")
	}

	var problems []string
	for _, r := range reports {
		if r.Replicas < want {
			problems = append(problems, fmt.Sprintf(
				"%s is on %d replicas, want %d: losing the node holding it takes it down cluster-wide",
				r.Bucket, r.Replicas, want))
			continue
		}
		// Configured, placed and serving are three facts. A stream can carry the
		// right count while its group is short of peers or those peers are not
		// caught up, and either reads as healthy right up until the node it is
		// really on goes away.
		if held := distinctPeers(r.Peers); held < want {
			problems = append(problems, fmt.Sprintf(
				"%s is configured for %d replicas but held by %d node(s) (%s)",
				r.Bucket, r.Replicas, held, strings.Join(r.Peers, ",")))
			continue
		}
		if r.Online < want {
			problems = append(problems, fmt.Sprintf(
				"%s is held by %d node(s) but only %d are caught up and reachable (leader %s)",
				r.Bucket, len(r.Peers), r.Online, r.Leader))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d of %d KV buckets are not replicated across the cluster's %d nodes:\n  %s",
			len(problems), len(reports), nodes, strings.Join(problems, "\n  "))
	}
	Detail(t, "kv_buckets", len(reports), "replicas", want, "nodes", nodes)
}

// RequireKVSingleNode fails unless a one-node cluster's buckets are exactly what
// one node can give them: one replica each, all of them serving.
//
// It is the counterpart assertion to RequireKVQuorum, and it is not the same
// check with a smaller number. On one node R1 is correct rather than a defect, so
// what has to be proved is the opposite: that nothing declared a count this
// deployment cannot place, and that a bucket which cannot be raised has not been
// left unreadable by the attempt.
func RequireKVSingleNode(t *testing.T, env *Env) {
	t.Helper()

	reports := KVReplication(t, env, 1)
	if len(reports) == 0 {
		t.Fatal("the cluster reports no KV buckets at all, so nothing was checked")
	}

	var problems []string
	for _, r := range reports {
		if r.Replicas != 1 {
			problems = append(problems, fmt.Sprintf(
				"%s is configured for %d replicas on a single node, which JetStream cannot place",
				r.Bucket, r.Replicas))
			continue
		}
		if !r.HasQuorum() {
			problems = append(problems, fmt.Sprintf("%s is not serving: %d of %d replicas online",
				r.Bucket, r.Online, r.Replicas))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d of %d KV buckets are wrong for a single-node cluster:\n  %s",
			len(problems), len(reports), strings.Join(problems, "\n  "))
	}
	Detail(t, "kv_buckets", len(reports), "replicas", 1, "nodes", 1)
}

// distinctPeers counts the unique node names holding a stream, since the leader
// is reported separately from the followers and could repeat.
func distinctPeers(peers []string) int {
	seen := make(map[string]struct{}, len(peers))
	for _, p := range peers {
		if p != "" {
			seen[p] = struct{}{}
		}
	}
	return len(seen)
}
