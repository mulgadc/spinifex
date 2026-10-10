//go:build e2e

package single

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runKVReplication proves what a single-node cluster's own state should look
// like, which is not the multi-node assertion with a smaller number in it.
//
// On one node R1 is the correct answer rather than the defect, so the thing worth
// proving is the opposite: that nothing asked for a replica count this deployment
// cannot place, that the operator surface says so plainly, and that repairing a
// cluster with nothing to repair changes nothing. The multi-node suites cannot
// cover any of that, because on three nodes the unplaceable count is the right
// one.
func runKVReplication(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — the cluster's own state on one node")

	harness.Step(t, "every bucket is on the one replica a single node can hold")
	harness.RequireKVSingleNode(t, fix.Env)

	harness.Step(t, "the operator surface reports a healthy single node")
	before, err := localReplicaReport(t)
	require.NoError(t, err,
		"spx admin kv replicas failed on a healthy single node, so nothing can be gated on it")
	require.NotEmpty(t, before, "the command reported no buckets at all, so nothing was checked")
	harness.Detail(t, "buckets", len(before), "want_replicas", before[0].Want)

	// Exit 0 is the contract a deployment gate reads. On one node every bucket is
	// at the count it should be and serving, so anything else is a bug in the
	// audit rather than in the cluster.
	require.Equal(t, 1, before[0].Want,
		"a single-node cluster wants one replica per bucket, not %d", before[0].Want)
	for _, r := range before {
		require.Truef(t, r.HasQuorum(),
			"%s reports %d of %d replicas online on a single node", r.Bucket, r.Online, r.Replicas)
	}

	// Twice, because --repair is documented as safe to run on a healthy cluster
	// and safe to run twice. A repair that rewrote a stream would show up as a
	// changed replica count or a bucket that stopped serving.
	harness.Step(t, "--repair is idempotent and changes nothing it should not")
	for i := 1; i <= 2; i++ {
		out, repairErr := runLocal(t, "sudo -n spx admin kv replicas --repair --json")
		require.NoErrorf(t, repairErr, "repair pass %d failed: %s", i, string(out))
	}

	after, err := localReplicaReport(t)
	require.NoError(t, err, "the audit stopped working after two repair passes")
	requireSameReplication(t, before, after)
	harness.Detail(t, "repair_passes", 2, "buckets", len(after))
}

// requireSameReplication fails if any bucket's replication changed across the
// repair passes, naming the bucket rather than reporting that two lists differ.
func requireSameReplication(t *testing.T, before, after []kvutil.BucketReport) {
	t.Helper()
	was := make(map[string]kvutil.BucketReport, len(before))
	for _, r := range before {
		was[r.Bucket] = r
	}
	require.Len(t, after, len(before), "the number of KV buckets changed across the repair passes")
	for _, now := range after {
		prev, ok := was[now.Bucket]
		require.Truef(t, ok, "%s appeared during a repair that should create nothing", now.Bucket)
		require.Equalf(t, prev.Replicas, now.Replicas,
			"%s went from %d replicas to %d across a repair that had nothing to repair",
			now.Bucket, prev.Replicas, now.Replicas)
		require.Truef(t, now.HasQuorum(),
			"%s stopped serving after a repair that had nothing to repair", now.Bucket)
	}
}

// localReplicaReport runs the replica audit on this node and decodes it. The
// suite runs on the node it is testing, so there is no ssh hop.
func localReplicaReport(t *testing.T) ([]kvutil.BucketReport, error) {
	t.Helper()
	out, err := runLocal(t, "sudo -n spx admin kv replicas --json")
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var reports []kvutil.BucketReport
	if jsonErr := json.Unmarshal(trimToJSON(out), &reports); jsonErr != nil {
		return nil, fmt.Errorf("decode replica report (%q): %w", string(out), jsonErr)
	}
	return reports, err
}

// runLocal runs a shell command on this node, returning its combined output
// whether or not it succeeded — the audit prints its report even when it exits
// non-zero, and a report naming a bucket is a better failure than an exit status.
func runLocal(t *testing.T, cmd string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
}

// trimToJSON drops anything before the JSON document, since sudo and the login
// shell can both write to the same stream.
func trimToJSON(out []byte) []byte {
	if i := strings.IndexAny(string(out), "[{"); i > 0 {
		return out[i:]
	}
	return out
}
