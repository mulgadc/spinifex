//go:build e2e

// Package teardown is the run-scoped cleanup sweep. It runs as
// the final suite of an e2e run: every resource the Ensure* fixtures created
// carries the e2e:run=<run-id> tag, and this suite reclaims anything still
// tagged — covering leaks from a crashed suite, an interrupted run, or a
// persistent (non-tofu-destroyed) cluster.
//
// It is intentionally tolerant: a sweep error is logged, never fatal. Most
// resources are already gone (each Ensure* registers its own teardown), so the
// common case deletes nothing. The value is the long tail of leaks.
package teardown

import (
	"os"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// TestSweepRunResources deletes every EC2/ELBv2 resource tagged with the
// current run id. Skips when SPINIFEX_E2E is unset (no cluster) or
// SPINIFEX_E2E_RUN_ID is unset (nothing to scope the sweep to — without a run
// id the sweep would no-op anyway, so skipping is clearer than running it).
func TestSweepRunResources(t *testing.T) {
	if os.Getenv("SPINIFEX_E2E") == "" {
		t.Skip("SPINIFEX_E2E unset; no cluster to sweep")
	}
	runID := os.Getenv(harness.RunIDEnv)
	if runID == "" {
		t.Skipf("%s unset; nothing to sweep", harness.RunIDEnv)
	}

	env := harness.LoadEnv(t)
	awsCli := harness.NewAWSClient(t, env)

	rep := harness.SweepRunResources(awsCli.EC2, awsCli.ELBv2, runID)

	for kind, ids := range rep.Deleted {
		t.Logf("swept %d %s: %v", len(ids), kind, ids)
	}
	for _, err := range rep.Errors {
		// Non-fatal: a delete failure usually means the resource is already
		// gone or still settling. Surface it for diagnostics, don't fail.
		t.Logf("sweep error (non-fatal): %v", err)
	}
}
