//go:build e2e

package multinode

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runInstanceDistribution launches the trio and checks at least two cluster nodes host them.
// Distribution is non-deterministic so all-on-one-node is a WARN not a failure, mirroring
// the bash driver behaviour.
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

	// spx get vms is best-effort: CLI ↔ NATS can race cluster join without
	// affecting the data path. Accept non-zero exit; log missing IDs as WARN.
	harness.Step(t, "spx get vms includes every launched instance (best-effort)")
	vms := harness.SpxRunBestEffort(t, "get", "vms")
	for _, id := range ids {
		if !strings.Contains(vms, id) {
			t.Logf("WARN: spx get vms did not list %s", id)
		}
	}
}
