//go:build e2e

package single

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// phase9b_FinalClusterStats is a single-node sanity pass against `spx get
// vms`. Replaces the umbrella's phase9 + phase9a (deleted) — resource
// cleanup is now owned by harness.Fixture.Close, invoked from TestMain
// after every Test* in the package has run.
func phase9b_FinalClusterStats(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 9b — Final Cluster Stats")
	if fix.Env.Mode != harness.ModeSingle {
		t.Skipf("Phase 9b is single-node only (mode=%s)", fix.Env.Mode)
	}

	out := harness.SpxGetVMs(t)
	harness.Detail(t, "spx_get_vms_bytes", len(out))

	// Best-effort sanity: log (don't fail) if the output unexpectedly lists
	// any i-XXXXXX rows. Per-test fixture cleanups should have terminated
	// every instance the suite launched in single-node mode, but other
	// suites running in parallel would surface here.
	if strings.Contains(out, "i-") {
		t.Logf("phase9b: spx get vms still references instance-like rows (may be from concurrent suites):\n%s", out)
	}
}
