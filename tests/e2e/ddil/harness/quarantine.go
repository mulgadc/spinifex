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

// Run wraps a scenario with reset-and-retry plus quarantine handling. It is
// the primary entry point used by the files in scenarios/:
//
//	harness.Run(t, cluster, ssh, "A", func(t *testing.T) { ... })
//
// Quarantine: scenarios whose letter appears in DDIL_QUARANTINED are skipped
// instead of executed. Per the design doc, a quarantined scenario must have
// a linked bead tracking its fix; the live status is tracked in
// tests/e2e/TEST_COVERAGE.md.
//
// Retry: delegates to harness.RetryWithReset so the shared package owns the
// reset-and-rerun mechanics; only quarantine policy stays DDIL-specific.
func Run(t *testing.T, c *harness.Cluster, ssh harness.SSH, letter string, fn func(*testing.T)) {
	t.Helper()

	if IsQuarantined(letter) {
		t.Skipf("QUARANTINED: scenario %s (DDIL_QUARANTINED=%s)", letter, os.Getenv("DDIL_QUARANTINED"))
		return
	}

	harness.RetryWithReset(t, c, ssh, "scenario "+letter, fn)
}

// IsQuarantined reports whether letter appears in DDIL_QUARANTINED.
// Comparison is case-insensitive; whitespace and empty entries are tolerated
// so the env var can be edited by humans without worrying about formatting.
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
