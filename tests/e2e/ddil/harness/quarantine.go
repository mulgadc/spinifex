//go:build e2e

// Package harness is DDIL's thin shim over the shared
// github.com/mulgadc/spinifex/tests/e2e/harness primitives. It hosts only the
// quarantine + retry wrapper that DDIL scenarios use; everything generic
// (Cluster, SSH, DaemonClient, ResetAllNodes, Witness, etc.) lives in the
// shared harness package.
package harness

import (
	"os"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Run wraps a scenario with reset-and-retry and quarantine handling.
// Scenarios listed in DDIL_QUARANTINED are skipped; others are delegated to
// harness.RetryWithReset which owns the reset-and-rerun mechanics.
func Run(t *testing.T, c *harness.Cluster, ssh harness.SSH, letter string, fn func(*testing.T)) {
	t.Helper()

	if IsQuarantined(letter) {
		t.Skipf("QUARANTINED: scenario %s (DDIL_QUARANTINED=%s)", letter, os.Getenv("DDIL_QUARANTINED"))
		return
	}

	harness.RetryWithReset(t, c, ssh, "scenario "+letter, fn)
}

// IsQuarantined reports whether letter appears in DDIL_QUARANTINED (case-insensitive, comma-separated).
func IsQuarantined(letter string) bool {
	raw := os.Getenv("DDIL_QUARANTINED")
	if raw == "" {
		return false
	}
	want := strings.ToUpper(strings.TrimSpace(letter))
	for _, part := range strings.Split(raw, ",") {
		if strings.ToUpper(strings.TrimSpace(part)) == want {
			return true
		}
	}
	return false
}
