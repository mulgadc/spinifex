//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
)

// TestMain amortises the embedded NATS+JetStream server across every test in
// this package instead of each StartGateway call booting (and tearing down)
// its own: standing one up is the dominant fixed cost of StartGateway.
//
// Per-test state isolation is preserved despite the shared server — see the
// accountAuthenticator doc comment in shared_nats.go for how.
func TestMain(m *testing.M) {
	policy, err := loadConformancePolicy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tests/integration: load AWS conformance policy:", err)
		os.Exit(1)
	}
	mode, err := conformanceModeFromEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tests/integration:", err)
		os.Exit(1)
	}

	h, err := startSharedNATS()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tests/integration: failed to start shared NATS server:", err)
		os.Exit(1)
	}
	sharedNATSHarness = h

	code := m.Run()

	// The predastore fixture outlives any single test on purpose, so this is
	// the only point at which its cluster goroutines can be drained.
	testpredastore.Stop()

	report := suiteConformance.report(policy, mode) + "\n" + suiteRequestConformance.report(policy, mode)
	fmt.Fprintln(os.Stderr, report)
	if blocking := suiteConformance.blocking(policy, mode); blocking != 0 {
		fmt.Fprintf(os.Stderr, "tests/integration: %d blocking AWS model conformance violation(s)\n", blocking)
		code = 1
	}
	if blocking := suiteRequestConformance.blocking(policy, mode); blocking != 0 {
		fmt.Fprintf(os.Stderr, "tests/integration: %d blocking AWS request conformance finding(s)\n", blocking)
		code = 1
	}
	if reportPath := os.Getenv("AWS_MODEL_CONFORMANCE_REPORT"); reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "tests/integration: create conformance report directory:", err)
			code = 1
		} else if err := os.WriteFile(reportPath, []byte(report+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "tests/integration: write conformance report:", err)
			code = 1
		}
	}

	// Connections are closed here, together, rather than per-test: closing
	// one early can kill the connection the clienttoken singleton latched
	// onto (see above), so every connection's lifetime is pinned to the
	// whole binary rather than to any single test.
	h.closeAll()
	h.srv.Shutdown()
	h.srv.WaitForShutdown()

	// Only reached on a clean exit; a killed run leaves the store for the
	// sweep in startSharedNATS to reclaim.
	h.removeStore()
	os.Exit(code)
}
