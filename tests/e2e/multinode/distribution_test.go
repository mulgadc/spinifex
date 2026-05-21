//go:build e2e

package multinode

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runInstanceDistribution is the Go port of instance lifecycle + distribution
// (run-multinode-e2e.sh:513-623). Launches a trio of instances and checks at
// least two cluster nodes host them. Distribution is non-deterministic
// (scheduler may stack on one node under load) so the bash version
// warns-not-fails when all three land on the same host. The Go port mirrors:
// minNodes=1 fatal, log "spread=N" so flaky CI surfaces in metrics without
// blocking the run.
func runInstanceDistribution(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Instance Distribution")

	ids := needInstanceTrio(t, fix)
	harness.Detail(t, "instances", ids)

	harness.Step(t, "check distribution across nodes")
	counts := harness.AssertSpreadAcrossNodes(t, fix.Cluster, ids, 1)
	harness.Detail(t, "spread", counts, "unique_hosts", len(counts))
	if len(counts) < 2 {
		t.Logf("WARN: all %d instances on one node (%v) — non-fatal, scheduler quirk", len(ids), counts)
	}

	// `spx get vms` is best-effort. Bash phase 3 invokes it with `2>/dev/null`
	// and downgrades a missing instance to WARN — the underlying CLI ↔ NATS
	// connection can race the cluster join (mulga-siv-90 CI run 26161792146)
	// without affecting correctness of the data path. Mirror that: accept
	// non-zero exit, log when an ID is missing rather than fail.
	harness.Step(t, "spx get vms includes every launched instance (best-effort)")
	vms := harness.SpxRunBestEffort(t, "get", "vms")
	for _, id := range ids {
		if !strings.Contains(vms, id) {
			t.Logf("WARN: spx get vms did not list %s", id)
		}
	}
}
