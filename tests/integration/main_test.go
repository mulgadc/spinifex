//go:build integration

package integration

import (
	"fmt"
	"os"
	"testing"
)

// TestMain amortises the embedded NATS+JetStream server across every test in
// this package instead of each StartGateway call booting (and tearing down)
// its own, for two reasons:
//
//  1. spinifex/gateway/ec2/instance's ClientTokenStore is a process-wide
//     sync.Once singleton bound to whichever *nats.Conn first initialises it.
//     In production a gateway holds one long-lived connection for its whole
//     life, so the singleton is correct there. But this test binary runs
//     every test in one process, so a per-test embedded server closes that
//     first connection the moment its owning test ends — every later
//     RunInstances-calling test then inherits a KV handle bound to a dead
//     connection. Keeping one server, and never closing an individual
//     test's connection early, keeps whichever connection wins the
//     singleton alive for the life of the whole binary.
//  2. It cuts per-test startup cost: standing up an embedded JetStream
//     server is the dominant fixed cost of StartGateway.
//
// Per-test state isolation is preserved despite the shared server — see the
// accountAuthenticator doc comment in shared_nats.go for how.
func TestMain(m *testing.M) {
	h, err := startSharedNATS()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tests/integration: failed to start shared NATS server:", err)
		os.Exit(1)
	}
	sharedNATSHarness = h

	code := m.Run()

	// Connections are closed here, together, rather than per-test: closing
	// one early can kill the connection the clienttoken singleton latched
	// onto (see above), so every connection's lifetime is pinned to the
	// whole binary rather than to any single test.
	h.closeAll()
	h.srv.Shutdown()
	h.srv.WaitForShutdown()
	os.Exit(code)
}
